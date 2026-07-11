// Package ec2sched starts and stops a single EC2 instance on a schedule.
//
// The logic is deliberately idempotent: it reads the instance's current state
// and only issues a start or stop when one is actually needed, so a schedule
// that fires against an instance already in the target state is a successful
// no-op rather than an error. Transient AWS failures and throttling are handled
// by the SDK's retryer, configured where the client is built (see Handle).
package ec2sched

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// Action is the operation a schedule performs.
type Action string

const (
	// Start powers the instance on.
	Start Action = "start"
	// Stop powers the instance off (the volume and instance are preserved).
	Stop Action = "stop"
)

// EC2API is the subset of the EC2 client this package uses. Declaring it as an
// interface lets tests substitute a fake without a live AWS account.
type EC2API interface {
	DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	StartInstances(context.Context, *ec2.StartInstancesInput, ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error)
	StopInstances(context.Context, *ec2.StopInstancesInput, ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error)
}

// Result is the structured outcome of a run, returned to Lambda and logged.
type Result struct {
	InstanceID    string `json:"instanceId"`
	Action        Action `json:"action"`
	PreviousState string `json:"previousState"`
	Changed       bool   `json:"changed"` // true when a start/stop was actually issued
	Message       string `json:"message"`
}

// ErrInstanceNotFound is returned when the instance ID matches no instance.
var ErrInstanceNotFound = errors.New("instance not found")

// Run reads the instance state and applies the action only if needed.
//
// It never treats "already in the desired state" as a failure: starting a
// running instance, or stopping a stopped one, returns Changed=false and no
// error. A terminated instance is an error, because it can be neither started
// nor stopped.
func Run(ctx context.Context, api EC2API, instanceID string, action Action, log *slog.Logger) (Result, error) {
	log = log.With("instanceId", instanceID, "action", string(action))

	state, err := currentState(ctx, api, instanceID)
	if err != nil {
		log.Error("could not read instance state", "error", err)
		return Result{}, err
	}
	log.Info("read instance state", "state", string(state))

	result := Result{InstanceID: instanceID, Action: action, PreviousState: string(state)}

	if state == types.InstanceStateNameTerminated || state == types.InstanceStateNameShuttingDown {
		return Result{}, fmt.Errorf("instance %s is %s and cannot be %sed", instanceID, state, action)
	}

	switch action {
	case Start:
		// pending is a start already in progress; running is done.
		if state == types.InstanceStateNameRunning || state == types.InstanceStateNamePending {
			result.Message = "already running or starting; nothing to do"
			log.Info(result.Message)
			return result, nil
		}
		if _, err := api.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: []string{instanceID}}); err != nil {
			log.Error("StartInstances failed", "error", err)
			return Result{}, fmt.Errorf("starting %s: %w", instanceID, err)
		}
		result.Changed, result.Message = true, "start requested"

	case Stop:
		// stopping is a stop already in progress; stopped is done.
		if state == types.InstanceStateNameStopped || state == types.InstanceStateNameStopping {
			result.Message = "already stopped or stopping; nothing to do"
			log.Info(result.Message)
			return result, nil
		}
		if _, err := api.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{instanceID}}); err != nil {
			log.Error("StopInstances failed", "error", err)
			return Result{}, fmt.Errorf("stopping %s: %w", instanceID, err)
		}
		result.Changed, result.Message = true, "stop requested"

	default:
		return Result{}, fmt.Errorf("unknown action %q", action)
	}

	log.Info(result.Message)
	return result, nil
}

// currentState returns the instance's current state name.
func currentState(ctx context.Context, api EC2API, instanceID string) (types.InstanceStateName, error) {
	out, err := api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{instanceID}})
	if err != nil {
		return "", fmt.Errorf("describing %s: %w", instanceID, err)
	}
	for _, reservation := range out.Reservations {
		for _, instance := range reservation.Instances {
			if instance.State != nil {
				return instance.State.Name, nil
			}
		}
	}
	return "", fmt.Errorf("%w: %s", ErrInstanceNotFound, instanceID)
}

// Handle is the Lambda body shared by both functions: read the instance ID from
// the environment, build a retrying EC2 client, and run the action. It is kept
// here so the two command entrypoints stay a few lines each.
func Handle(ctx context.Context, action Action) (Result, error) {
	instanceID := os.Getenv("INSTANCE_ID")
	if instanceID == "" {
		return Result{}, errors.New("INSTANCE_ID environment variable is not set")
	}

	// Adaptive retry backs off under throttling and retries transient errors;
	// max attempts bounds how long a single invocation will keep trying.
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRetryMode(aws.RetryModeAdaptive),
		config.WithRetryMaxAttempts(5),
	)
	if err != nil {
		return Result{}, fmt.Errorf("loading AWS config: %w", err)
	}

	return Run(ctx, ec2.NewFromConfig(cfg), instanceID, action, slog.Default())
}
