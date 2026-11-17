// Reading a class's striping parameters, stripe_width and stripe_size.
//
// It lives here because two components read them and have to agree to the
// byte: the CSI driver, which provisions from them, and the operator, which
// checks a class assigned to a pool against the pool's volume defaults. Two
// parsers would disagree first about the spellings of a size, 64Ki against
// 65536, and a check that compared strings would call those different.

package kube

import (
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/simplyblock/atlas/lvol"
)

const (
	// DefaultStripeChunkBytes is the chunk each member takes before the next one
	// does, when a class names none. 64KiB is LVM's own default and keeps a
	// 4KiB random write on one member.
	DefaultStripeChunkBytes = int64(64 << 10)

	// MinStripeChunkBytes and MaxStripeChunkBytes bound stripe_size. LVM takes
	// any power of two from the page size up. Past 1MiB a stripe stops spreading
	// the I/O of a single request at all, which is the reason to stripe.
	MinStripeChunkBytes = int64(4 << 10)
	MaxStripeChunkBytes = int64(1 << 20)
)

// Stripe is what a class asks of a striped volume. Width 1 is an ordinary
// volume, and ChunkBytes is then zero.
type Stripe struct {
	Width      int
	ChunkBytes int64
}

// Striped reports whether the class asked for a striped volume at all.
func (s Stripe) Striped() bool {
	return s.Width > 1
}

// ParseStripe reads stripe_width and stripe_size from a class's parameters.
//
// An absent, empty, 0, or 1 width is an ordinary volume, and stripe_size is then
// not read at all, so a class that never asked for striping reads exactly as one
// from before striping existed. Anything else that is not a width from 2 to
// lvol.MaxStripeWidth, or a chunk that is not a power of two from 4KiB to 1MiB,
// is an error naming the parameter.
func ParseStripe(params map[string]string) (Stripe, error) {
	raw := strings.TrimSpace(params[ParamStripeWidth])
	if raw == "" {
		return Stripe{Width: 1}, nil
	}
	width, err := strconv.Atoi(raw)
	if err != nil || width < 0 {
		return Stripe{}, fmt.Errorf("%s must be a whole number of storage nodes, not %q", ParamStripeWidth, raw)
	}
	if width <= 1 {
		return Stripe{Width: 1}, nil
	}
	if width > lvol.MaxStripeWidth {
		return Stripe{}, fmt.Errorf("%s is %d, and LVM stripes across at most %d members",
			ParamStripeWidth, width, lvol.MaxStripeWidth)
	}

	stripe := Stripe{Width: width, ChunkBytes: DefaultStripeChunkBytes}
	if rawChunk := strings.TrimSpace(params[ParamStripeSize]); rawChunk != "" {
		q, err := resource.ParseQuantity(rawChunk)
		if err != nil {
			return Stripe{}, fmt.Errorf("%s must be a size such as 64Ki, not %q: %w", ParamStripeSize, rawChunk, err)
		}
		chunk := q.Value()
		if chunk < MinStripeChunkBytes || chunk > MaxStripeChunkBytes || chunk&(chunk-1) != 0 {
			return Stripe{}, fmt.Errorf("%s is %d bytes, and has to be a power of two from 4Ki to 1Mi",
				ParamStripeSize, chunk)
		}
		stripe.ChunkBytes = chunk
	}
	return stripe, nil
}
