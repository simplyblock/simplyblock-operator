// Tests for reading a class's striping parameters.
package kube

import "testing"

func TestParseStripe(t *testing.T) {
	for name, tc := range map[string]struct {
		params map[string]string
		want   Stripe
	}{
		"absent":                 {map[string]string{}, Stripe{Width: 1}},
		"zero":                   {map[string]string{ParamStripeWidth: "0"}, Stripe{Width: 1}},
		"one ignores the size":   {map[string]string{ParamStripeWidth: "1", ParamStripeSize: "junk"}, Stripe{Width: 1}},
		"width with the default": {map[string]string{ParamStripeWidth: "4"}, Stripe{Width: 4, ChunkBytes: 64 << 10}},
		"binary suffix":          {map[string]string{ParamStripeWidth: "2", ParamStripeSize: "128Ki"}, Stripe{Width: 2, ChunkBytes: 128 << 10}},
		"plain bytes":            {map[string]string{ParamStripeWidth: "2", ParamStripeSize: "65536"}, Stripe{Width: 2, ChunkBytes: 64 << 10}},
	} {
		got, err := ParseStripe(tc.params)
		if err != nil || got != tc.want {
			t.Errorf("%s: ParseStripe(%v) = %+v, %v; want %+v", name, tc.params, got, err, tc.want)
		}
	}
	for _, params := range []map[string]string{
		{ParamStripeWidth: "two"},
		{ParamStripeWidth: "-2"},
		{ParamStripeWidth: "129"},
		{ParamStripeWidth: "2", ParamStripeSize: "96Ki"},
		{ParamStripeWidth: "2", ParamStripeSize: "2Ki"},
		{ParamStripeWidth: "2", ParamStripeSize: "2Mi"},
		{ParamStripeWidth: "2", ParamStripeSize: "big"},
	} {
		if got, err := ParseStripe(params); err == nil {
			t.Errorf("ParseStripe(%v) = %+v, want an error", params, got)
		}
	}
}
