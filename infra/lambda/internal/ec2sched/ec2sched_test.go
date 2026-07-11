package ec2sched

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// fakeEC2 is an in-memory EC2 API. It reports a fixed state and records whether
// start/stop were called, so a test can assert both the outcome and the action.
type fakeEC2 struct {
	state       types.InstanceStateName
	noInstance  bool
	describeErr error
	startErr    error
	stopErr     error

	started bool
	stopped bool
}

func (f *fakeEC2) DescribeInstances(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	if f.noInstance {
		return &ec2.DescribeInstancesOutput{}, nil
	}
	return &ec2.DescribeInstancesOutput{
		Reservations: []types.Reservation{{
			Instances: []types.Instance{{State: &types.InstanceState{Name: f.state}}},
		}},
	}, nil
}

func (f *fakeEC2) StartInstances(_ context.Context, _ *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
	if f.startErr != nil {
		return nil, f.startErr
	}
	f.started = true
	return &ec2.StartInstancesOutput{}, nil
}

func (f *fakeEC2) StopInstances(_ context.Context, _ *ec2.StopInstancesInput, _ ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
	if f.stopErr != nil {
		return nil, f.stopErr
	}
	f.stopped = true
	return &ec2.StopInstancesOutput{}, nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func run(t *testing.T, f *fakeEC2, action Action) (Result, error) {
	t.Helper()
	return Run(context.Background(), f, "i-0123456789abcdef0", action, discardLogger())
}

func TestStart(t *testing.T) {
	tests := []struct {
		name        string
		state       types.InstanceStateName
		wantChanged bool
		wantStarted bool
	}{
		{"stopped starts", types.InstanceStateNameStopped, true, true},
		{"stopping starts", types.InstanceStateNameStopping, true, true},
		// Already running or on its way: no action, still a success.
		{"running is a no-op", types.InstanceStateNameRunning, false, false},
		{"pending is a no-op", types.InstanceStateNamePending, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeEC2{state: tt.state}
			res, err := run(t, f, Start)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Changed != tt.wantChanged {
				t.Errorf("Changed = %v, want %v", res.Changed, tt.wantChanged)
			}
			if f.started != tt.wantStarted {
				t.Errorf("StartInstances called = %v, want %v", f.started, tt.wantStarted)
			}
			if res.PreviousState != string(tt.state) {
				t.Errorf("PreviousState = %q, want %q", res.PreviousState, tt.state)
			}
		})
	}
}

func TestStop(t *testing.T) {
	tests := []struct {
		name        string
		state       types.InstanceStateName
		wantChanged bool
		wantStopped bool
	}{
		{"running stops", types.InstanceStateNameRunning, true, true},
		{"pending stops", types.InstanceStateNamePending, true, true},
		{"stopped is a no-op", types.InstanceStateNameStopped, false, false},
		{"stopping is a no-op", types.InstanceStateNameStopping, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeEC2{state: tt.state}
			res, err := run(t, f, Stop)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Changed != tt.wantChanged {
				t.Errorf("Changed = %v, want %v", res.Changed, tt.wantChanged)
			}
			if f.stopped != tt.wantStopped {
				t.Errorf("StopInstances called = %v, want %v", f.stopped, tt.wantStopped)
			}
		})
	}
}

// Idempotency, stated as a property: running Start twice issues at most one
// StartInstances call across the two runs against the resulting states.
func TestStartIsIdempotent(t *testing.T) {
	// First run against a stopped instance starts it.
	f := &fakeEC2{state: types.InstanceStateNameStopped}
	if res, _ := run(t, f, Start); !res.Changed {
		t.Fatal("first Start should act")
	}
	// A second run, now that it is running, must not act again.
	f2 := &fakeEC2{state: types.InstanceStateNameRunning}
	if res, _ := run(t, f2, Start); res.Changed {
		t.Error("second Start against a running instance should be a no-op")
	}
}

func TestTerminatedIsAnError(t *testing.T) {
	for _, state := range []types.InstanceStateName{types.InstanceStateNameTerminated, types.InstanceStateNameShuttingDown} {
		f := &fakeEC2{state: state}
		if _, err := run(t, f, Start); err == nil {
			t.Errorf("Start against a %s instance should error", state)
		}
		if f.started {
			t.Errorf("a %s instance must not be started", state)
		}
	}
}

func TestInstanceNotFound(t *testing.T) {
	f := &fakeEC2{noInstance: true}
	_, err := run(t, f, Start)
	if !errors.Is(err, ErrInstanceNotFound) {
		t.Errorf("Run = %v, want ErrInstanceNotFound", err)
	}
}

// A describe failure must surface, not be swallowed — otherwise a schedule
// could silently do nothing.
func TestDescribeErrorSurfaces(t *testing.T) {
	f := &fakeEC2{describeErr: errors.New("throttled")}
	if _, err := run(t, f, Stop); err == nil {
		t.Error("a DescribeInstances failure should be returned")
	}
}

func TestStartErrorSurfaces(t *testing.T) {
	f := &fakeEC2{state: types.InstanceStateNameStopped, startErr: errors.New("boom")}
	_, err := run(t, f, Start)
	if err == nil {
		t.Fatal("a StartInstances failure should be returned")
	}
	if !errors.Is(err, f.startErr) {
		t.Errorf("error %v should wrap the underlying failure", err)
	}
}

func TestUnknownAction(t *testing.T) {
	f := &fakeEC2{state: types.InstanceStateNameStopped}
	if _, err := Run(context.Background(), f, "i-x", Action("restart"), discardLogger()); err == nil {
		t.Error("an unknown action should error")
	}
}
