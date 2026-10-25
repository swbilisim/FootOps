package install

import (
	"context"
	"errors"
	"io"
	"testing"
)

type fakeStep struct {
	name     string
	done     bool
	applyErr error
	applied  bool
}

func (f *fakeStep) Name() string                        { return f.name }
func (f *fakeStep) Describe() string                    { return "fake" }
func (f *fakeStep) Check(context.Context) (bool, error) { return f.done, nil }
func (f *fakeStep) Apply(context.Context) error {
	f.applied = true
	return f.applyErr
}

func TestRunSkipsCompletedSteps(t *testing.T) {
	a := &fakeStep{name: "a", done: true}
	b := &fakeStep{name: "b"}
	res, err := Run(context.Background(), []Step{a, b}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if a.applied {
		t.Error("completed step was applied again")
	}
	if !b.applied {
		t.Error("pending step was not applied")
	}
	if !res[0].Skipped || res[1].Skipped {
		t.Errorf("unexpected skip flags: %+v", res)
	}
}

func TestRunStopsAtFirstFailure(t *testing.T) {
	boom := errors.New("boom")
	a := &fakeStep{name: "a", applyErr: boom}
	b := &fakeStep{name: "b"}
	res, err := Run(context.Background(), []Step{a, b}, io.Discard)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if b.applied {
		t.Error("step after failure was applied")
	}
	if len(res) != 1 {
		t.Errorf("got %d results, want 1", len(res))
	}
}
