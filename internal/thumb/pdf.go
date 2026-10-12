package thumb

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	"github.com/tetratelabs/wazero"

	"github.com/krishna2206/zefile/internal/storage"
)

// A PDF's first page is rasterised in-process by PDFium run under wazero
//
// The cost is about 5 MB of embedded module and a one-off compile of it on
// first use

const pdfTimeout = time.Minute
const pdfSandboxDir = "/tmp"

var (
	pdfOnce sync.Once
	pdfPool pdfium.Pool
	pdfErr  error
)

// builds the renderer on first use and reuses it after
func pdfiumPool() (pdfium.Pool, error) {
	pdfOnce.Do(func() {
		pdfPool, pdfErr = webassembly.Init(webassembly.Config{
			MaxIdle:  1,
			MaxTotal: 1,

			// only the temp directory holding the staged copy is visible
			FSConfig: wazero.NewFSConfig().
				WithReadOnlyDirMount(os.TempDir(), pdfSandboxDir),

			Stdout: io.Discard,
			Stderr: io.Discard,
		})
		if pdfErr != nil {
			pdfErr = fmt.Errorf("thumb: pdfium init: %w", pdfErr)
		}
	})
	return pdfPool, pdfErr
}

// render the first page to jpg
func runPDFium(ctx context.Context, src, dst string) error {
	pool, err := pdfiumPool()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, pdfTimeout)
	defer cancel()

	instance, err := pool.GetInstanceWithContext(ctx)
	if err != nil {
		return fmt.Errorf("thumb: pdfium instance: %w", err)
	}
	defer func() { _ = instance.Close() }()

	sandboxed := path.Join(pdfSandboxDir, filepath.Base(src))
	doc, err := instance.OpenDocument(&requests.OpenDocument{FilePath: &sandboxed})
	if err != nil {
		return fmt.Errorf("thumb: pdfium open: %w", err)
	}

	out, err := instance.RenderToFile(&requests.RenderToFile{
		RenderPageInPixels: &requests.RenderPageInPixels{
			Page: requests.Page{ByIndex: &requests.PageByIndex{
				Document: doc.Document,
				Index:    0,
			}},
			Width:  maxSide,
			Height: maxSide,
		},
		OutputFormat:  requests.RenderToFileOutputFormatJPG,
		OutputTarget:  requests.RenderToFileOutputTargetBytes,
		OutputQuality: quality,
		MaxFileSize:   storage.MaxThumbBytes,
	})
	if err != nil {
		return fmt.Errorf("thumb: pdfium render: %w", err)
	}
	if out.ImageBytes == nil {
		return fmt.Errorf("thumb: pdfium rendered no image")
	}
	return os.WriteFile(dst, *out.ImageBytes, 0o600)
}
