// The `mds-runner extents` report. The guest has no shell, so a harness that
// needs to know what a byte range of a file in an export is made of asks the
// guest agent through the runner, which shares the guest's private network.

package runner

import (
	"context"
	"encoding/json"
	"io"

	"github.com/simplyblock/atlas/fiemap"
	"github.com/simplyblock/atlas/nfsexport"
)

// ExtentAsker is the guest agent's FileExtents. nfsexportrpc.Client satisfies it.
type ExtentAsker interface {
	FileExtents(ctx context.Context, exportPath, file string, offset, length uint64) (nfsexport.FileExtents, error)
}

// ExtentReport is what `mds-runner extents` prints.
type ExtentReport struct {
	Export string         `json:"export"`
	File   string         `json:"file"`
	Offset uint64         `json:"offset"`
	Length uint64         `json:"length"`
	Size   uint64         `json:"size"`
	Pieces []fiemap.Piece `json:"pieces"`
}

// WriteExtentReport asks the agent and writes its answer to w as JSON.
func WriteExtentReport(
	ctx context.Context, asker ExtentAsker, w io.Writer, exportPath, file string, offset, length uint64,
) error {
	got, err := asker.FileExtents(ctx, exportPath, file, offset, length)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(ExtentReport{
		Export: exportPath, File: file, Offset: offset, Length: length, Size: got.Size, Pieces: got.Pieces,
	})
}
