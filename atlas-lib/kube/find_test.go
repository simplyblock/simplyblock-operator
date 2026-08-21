package kube

import (
	"errors"
	"testing"

	"github.com/simplyblock/atlas/errs"
)

func TestFindByKey(t *testing.T) {
	type item struct {
		id   string
		name string
	}
	items := []item{{"a", "alpha"}, {"b", "beta"}}
	keyFn := func(i item) string { return i.id }

	got, err := FindByKey(items, keyFn, "b")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.name != "beta" {
		t.Errorf("got %q, want beta", got.name)
	}

	if _, err := FindByKey(items, keyFn, "missing"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("missing key: err = %v, want ErrNotFound", err)
	}

	if _, err := FindByKey([]item(nil), keyFn, "a"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("empty items: err = %v, want ErrNotFound", err)
	}
}
