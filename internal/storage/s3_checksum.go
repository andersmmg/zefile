package storage

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Whole-object checksums on object storage.
//
// Only single-shot PutObject writes carry a whole-object SHA-256 computed by
// the store. Multipart uploads produce composite checksums (digests of parts,
// not the file), and CopyObject checksums are ignored by providers like B2.
//
// Therefore, a stored whole-object SHA-256 comes only from single-shot writes.
// The upload path handles multipart files via the browser's digest (see
// [checksum.Service.StoreClaim]), and [S3.ObjectChecksum] returns only
// verified whole-object values.

var _ ChecksumStore = (*S3)(nil)

// ObjectChecksum implements [ChecksumStore]: the checksum comes straight
// from the object's own metadata, so the answer costs one HEAD.
func (s *S3) ObjectChecksum(ctx context.Context, p Path) (string, error) {
	if err := authorize(ctx, s.guard, OpRead, p); err != nil {
		return "", err
	}
	out, err := s.client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket:       aws.String(s.bucket),
		Key:          aws.String(s.key(p)),
		ChecksumMode: types.ChecksumModeEnabled,
	})
	if err != nil {
		return "", mapS3Err(err)
	}
	sum, err := wholeObjectSHA256(out)
	if err != nil {
		// The one read-side case worth a trace: the file exists but the
		// store answers this HEAD without a usable whole-object SHA-256.
		slog.InfoContext(ctx, "checksum: store holds no whole-object SHA-256",
			"path", p.String(), "checksum_type", string(out.ChecksumType),
			"header_present", aws.ToString(out.ChecksumSHA256) != "")
		return "", err
	}
	return sum, nil
}

// wholeObjectSHA256 extracts the header's base64 value as lowercase hex,
// refusing anything that is not a whole-object SHA-256.
func wholeObjectSHA256(out *awss3.HeadObjectOutput) (string, error) {
	return decodeWholeObjectSHA256(aws.ToString(out.ChecksumSHA256), out.ChecksumType)
}

// decodeWholeObjectSHA256 is the shared gate. A composite value ends in
// "-<part count>" and digests the parts' digests; handing it out as the
// file's SHA-256 would be a lie.
func decodeWholeObjectSHA256(sum string, typ types.ChecksumType) (string, error) {
	if sum == "" || typ == types.ChecksumTypeComposite || strings.Contains(sum, "-") {
		return "", fmt.Errorf("storage: s3: store holds no whole-object sha256 (type %q, header %t): %w",
			string(typ), sum != "", ErrNotSupported)
	}
	raw, err := base64.StdEncoding.DecodeString(sum)
	if err != nil || len(raw) != sha256.Size {
		return "", fmt.Errorf("storage: s3: unusable checksum %q: %w", sum, ErrNotSupported)
	}
	return hex.EncodeToString(raw), nil
}
