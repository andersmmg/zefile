package storage

import (
	"context"
	"io"
	"os"
	"time"
)

// Optional backend capabilities beyond [FS], discovered by type assertion
// where they are used. A backend that lacks one is adapted by the As*
// helpers below into a refusal answering [ErrNotSupported].

// Stager is the lifecycle of a partial upload: bytes accumulate under an
// identifier until committed to a path in one step, or discarded.
type Stager interface {
	// NewStage creates an empty staged file and returns its identifier.
	NewStage(ctx context.Context) (StageID, error)

	// EnsureStage opens a stage under a caller-chosen identifier, creating it
	// empty if absent, and returns how many bytes it already holds.
	EnsureStage(ctx context.Context, id StageID) (int64, error)

	// StageSize reports how many bytes a staged file holds: where a client
	// resumes from.
	StageSize(ctx context.Context, id StageID) (int64, error)

	// AppendToStage writes at offset and returns the new size.
	AppendToStage(ctx context.Context, id StageID, offset int64, src io.Reader) (int64, error)

	// CommitStage publishes a completed stage to its destination, refusing a
	// destination that already exists. Authorisation runs here, when the
	// bytes become visible.
	CommitStage(ctx context.Context, id StageID, to Path) error

	// OpenStage opens a staged file for reading (e.g. checksums before publish).
	OpenStage(ctx context.Context, id StageID) (File, error)

	// DiscardStage deletes a stage. It is idempotent.
	DiscardStage(ctx context.Context, id StageID) error

	// PurgeStagesOlderThan deletes stages untouched since the cutoff.
	PurgeStagesOlderThan(ctx context.Context, cutoff time.Time) (int, error)
}

// DirectStager receives an upload's bytes without the server seeing them:
// the backend mints capability URLs, the client writes straight to
// storage, and the server tracks progress from metadata alone. Its
// presence switches the upload API to direct mode; without it, uploads
// stage through [Stager].
type DirectStager interface {
	// BeginUpload opens an upload to target of the declared size. The
	// result is invisible to readers until CompleteUpload.
	BeginUpload(ctx context.Context, target Path, size int64) (UploadHandle, error)

	// PresignParts returns one capability URL per requested 1-based part number.
	PresignParts(ctx context.Context, h UploadHandle, partNumbers []int) ([]PartURL, error)

	// CommittedBytes reports how many bytes storage holds; clients resume from here.
	CommittedBytes(ctx context.Context, h UploadHandle) (int64, error)

	// CompleteUpload publishes the upload to its destination, refusing a
	// destination that already exists.
	CompleteUpload(ctx context.Context, h UploadHandle, to Path) error

	// AbortUpload discards everything received. It is idempotent.
	AbortUpload(ctx context.Context, h UploadHandle) error
}

// MaxMultipartParts caps parts per multipart upload; part sizes grow to stay under it.
const MaxMultipartParts = 10000

// DefaultPartSize is the multipart part length when none is configured.
// Large enough that a fast uplink spends its time sending bytes rather than
// per-request round trips; the client flies several parts concurrently, so
// this is the unit of parallelism, not of serialisation.
const DefaultPartSize = 64 << 20

// PartSizeFor resolves the part length for a transfer: the configured size,
// grown so even the largest declared size fits under [MaxMultipartParts].
func PartSizeFor(configured, size int64) int64 {
	need := (size + MaxMultipartParts - 1) / MaxMultipartParts
	if need < 1 {
		need = 1
	}
	if configured < need {
		return need
	}
	return configured
}

// UploadHandle identifies an in-progress direct upload. ID is opaque and
// never appears in a URL.
type UploadHandle struct {
	ID string

	// PartSize is the byte length every part except the last must carry.
	PartSize int64
}

// PartURL is one presigned part request. The tags are the API contract.
type PartURL struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

// Trasher is reversible deletion: entries move aside restorable, and
// purging removes them for good. On [Local] these are renames; on object
// storage they are server-side copies.
type Trasher interface {
	// Trash moves an entry into the trash and returns its new identifier.
	Trash(ctx context.Context, p Path) (TrashID, error)

	// Restore moves a trashed entry back to a destination that must not
	// already exist.
	Restore(ctx context.Context, id TrashID, to Path) error

	// PurgeTrash deletes a trashed entry for good. It is idempotent.
	PurgeTrash(ctx context.Context, id TrashID) error

	// TrashStat reports the size and kind of a trashed entry.
	TrashStat(ctx context.Context, id TrashID) (os.FileInfo, error)
}

// TreeCopier copies a file or an entire directory tree, with no size
// limit, reported through progress. It runs in the job worker, never in
// a request.
type TreeCopier interface {
	CopyTree(ctx context.Context, from, to Path, progress func(done, total int64)) error
}

// Extractor unpacks a ZIP archive into a new directory under dest. The
// safety ceilings (entry count, expanded bytes, ratio) are the backend's
// to enforce.
type Extractor interface {
	ExtractZip(ctx context.Context, archive, dest Path, progress func(done, total int64)) (Path, error)
} // Compile-time checks that Local keeps every capability it offers.
var (
	_ Stager     = (*Local)(nil)
	_ Trasher    = (*Local)(nil)
	_ TreeCopier = (*Local)(nil)
	_ Extractor  = (*Local)(nil)
)

// Presigner mints short-lived URLs fetching a file straight from
// storage, so the content origin can redirect instead of streaming.
type Presigner interface {
	// PresignGet returns a URL fetching the file at p, valid for ttl. When
	// head is true it answers HEAD rather than GET. disposition is a
	// per-request Content-Disposition override ("" keeps the stored one).
	PresignGet(ctx context.Context, p Path, head bool, ttl time.Duration, disposition string) (string, error)
}

// The As* helpers adapt a backend to the capability a caller needs,
// returning a refusal answering [ErrNotSupported] when it lacks one.
func AsStager(fs FS) Stager {
	if s, ok := fs.(Stager); ok {
		return s
	}
	return refuseStager{}
}

func AsTrasher(fs FS) Trasher {
	if t, ok := fs.(Trasher); ok {
		return t
	}
	return refuseTrasher{}
}

func AsDirectStager(fs FS) DirectStager {
	if d, ok := fs.(DirectStager); ok {
		return d
	}
	return refuseDirectStager{}
}

func AsTreeCopier(fs FS) TreeCopier {
	if c, ok := fs.(TreeCopier); ok {
		return c
	}
	return refuseTreeCopier{}
}

func AsExtractor(fs FS) Extractor {
	if e, ok := fs.(Extractor); ok {
		return e
	}
	return refuseExtractor{}
}

// Refusal implementations answering [ErrNotSupported] on every method.
type refuseStager struct{}

func (refuseStager) NewStage(context.Context) (StageID, error) {
	return "", ErrNotSupported
}
func (refuseStager) EnsureStage(context.Context, StageID) (int64, error) {
	return 0, ErrNotSupported
}
func (refuseStager) StageSize(context.Context, StageID) (int64, error) {
	return 0, ErrNotSupported
}
func (refuseStager) AppendToStage(context.Context, StageID, int64, io.Reader) (int64, error) {
	return 0, ErrNotSupported
}
func (refuseStager) CommitStage(context.Context, StageID, Path) error {
	return ErrNotSupported
}
func (refuseStager) OpenStage(context.Context, StageID) (File, error) {
	return nil, ErrNotSupported
}
func (refuseStager) DiscardStage(context.Context, StageID) error {
	return ErrNotSupported
}
func (refuseStager) PurgeStagesOlderThan(context.Context, time.Time) (int, error) {
	return 0, ErrNotSupported
}

type refuseTrasher struct{}

func (refuseTrasher) Trash(context.Context, Path) (TrashID, error) {
	return "", ErrNotSupported
}
func (refuseTrasher) Restore(context.Context, TrashID, Path) error {
	return ErrNotSupported
}
func (refuseTrasher) PurgeTrash(context.Context, TrashID) error {
	return ErrNotSupported
}
func (refuseTrasher) TrashStat(context.Context, TrashID) (os.FileInfo, error) {
	return nil, ErrNotSupported
}

type refuseDirectStager struct{}

func (refuseDirectStager) BeginUpload(context.Context, Path, int64) (UploadHandle, error) {
	return UploadHandle{}, ErrNotSupported
}
func (refuseDirectStager) PresignParts(context.Context, UploadHandle, []int) ([]PartURL, error) {
	return nil, ErrNotSupported
}
func (refuseDirectStager) CommittedBytes(context.Context, UploadHandle) (int64, error) {
	return 0, ErrNotSupported
}
func (refuseDirectStager) CompleteUpload(context.Context, UploadHandle, Path) error {
	return ErrNotSupported
}
func (refuseDirectStager) AbortUpload(context.Context, UploadHandle) error {
	return ErrNotSupported
}

type refuseTreeCopier struct{}

func (refuseTreeCopier) CopyTree(context.Context, Path, Path, func(done, total int64)) error {
	return ErrNotSupported
}

type refuseExtractor struct{}

func (refuseExtractor) ExtractZip(context.Context, Path, Path, func(done, total int64)) (Path, error) {
	return Path{}, ErrNotSupported
}
