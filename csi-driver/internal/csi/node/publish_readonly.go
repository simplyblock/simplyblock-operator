// Publishing a volume read-only when the pod asks for it (readOnly: true on the
// pod's volume, which kubelet passes as NodePublishVolumeRequest.readonly).
//
// A filesystem is bind-mounted ro. A raw-block volume gets a read-only
// dm-linear mapping of its own, over the staged device, and that mapping is
// what the target is bound to. The staged device itself stays as it is: other
// pods on the node may be using the same ReadWriteOnce volume writable, and a
// ReadOnlyMany volume is read-only there already (readonly.go).
//
// The mapping is fixed to the size the volume had when it was published. An
// expansion grows the staged stack, and a read-only consumer sees the new size
// after its next publish.

package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/simplyblock/atlas/devmapper"
)

// readOnlyPublishPrefix starts every read-only publish mapping's name. It is
// not the sb- a volume's own mapping carries, which the swap service claims.
const readOnlyPublishPrefix = "sbro-"

// publishMapper is the device-mapper surface a read-only raw-block publish
// uses; *devmapper.Mapper satisfies it.
type publishMapper interface {
	Sectors(ctx context.Context, device string) (uint64, error)
	CreateReadOnly(ctx context.Context, name, device string, sectors uint64) error
	ReadOnly(ctx context.Context, name string) (bool, error)
	Remove(ctx context.Context, name string) error
}

func (ns *Server) readOnlyMapper() publishMapper {
	if ns.publishMapper != nil {
		return ns.publishMapper
	}
	return devmapper.New(nil)
}

// readOnlyPublishName names the mapping of one publish: one volume bound at one
// target. Both are in every publish and unpublish request, so the name is
// derived rather than recorded.
func readOnlyPublishName(volumeID, targetPath string) string {
	sum := sha256.Sum256([]byte(volumeID + "\x00" + targetPath))
	return readOnlyPublishPrefix + hex.EncodeToString(sum[:8])
}

// readOnlyDevice returns the read-only mapping over device for this publish,
// creating it the first time. A host without device-mapper has no read-only
// device to offer, and a writable one is not what the pod asked for.
func (ns *Server) readOnlyDevice(ctx context.Context, volumeID, targetPath, device string) (string, error) {
	if !ns.deviceMapper {
		return "", status.Error(codes.FailedPrecondition,
			"a raw-block volume published readOnly needs device-mapper, which this host does not offer")
	}
	mapper := ns.readOnlyMapper()
	name := readOnlyPublishName(volumeID, targetPath)
	readOnly, err := mapper.ReadOnly(ctx, name)
	switch {
	case err == nil && readOnly:
		return devmapper.Path(name), nil
	case err == nil:
		return "", fmt.Errorf("mapping %s for a read-only publish is writable", name)
	case !errors.Is(err, devmapper.ErrNotFound):
		return "", err
	}
	sectors, err := mapper.Sectors(ctx, device)
	if err != nil {
		return "", err
	}
	if err := mapper.CreateReadOnly(ctx, name, device, sectors); err != nil {
		return "", err
	}
	return devmapper.Path(name), nil
}

// releaseReadOnlyDevice removes the read-only mapping of one publish, which is
// success when there is none: a filesystem or a writable publish never made
// one.
func (ns *Server) releaseReadOnlyDevice(ctx context.Context, volumeID, targetPath string) error {
	if !ns.deviceMapper {
		return nil
	}
	return ns.readOnlyMapper().Remove(ctx, readOnlyPublishName(volumeID, targetPath))
}
