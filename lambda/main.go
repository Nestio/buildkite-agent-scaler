package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"math"
	"os"
	"sync"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/buildkite/buildkite-agent-scaler/buildkite"
	"github.com/buildkite/buildkite-agent-scaler/scaler"
	"github.com/buildkite/buildkite-agent-scaler/version"
)

// Stores the last time we scaled in/out in global lambda state
// On a cold start this will be reset to a zero value
var (
	lastScaleMu               sync.Mutex
	lastScaleTimesFetched     bool
	lastScaleIn, lastScaleOut time.Time
)

func main() {
	if EnvBool("DEBUG") {
		_, err := Handler(context.Background(), json.RawMessage([]byte{}))
		if err != nil {
			log.Fatal(err)
		}
		return
	}
	lambda.Start(Handler)
}

func Handler(ctx context.Context, evt json.RawMessage) (string, error) {
	log.Printf("buildkite-agent-scaler version %s", version.VersionString())

	// Required environment variables
	buildkiteQueue := RequireEnvString("BUILDKITE_QUEUE")
	asgName := RequireEnvString("ASG_NAME")
	agentsPerInstance := RequireEnvInt("AGENTS_PER_INSTANCE")

	// Optional environment variables (but they must parse correctly if set).
	interval := EnvDuration("LAMBDA_INTERVAL", 10*time.Second)

	timeoutDuration := EnvDuration("LAMBDA_TIMEOUT", -1)
	var timeout <-chan time.Time
	if timeoutDuration >= 0 {
		timeout = time.After(timeoutDuration)
	}

	asgActivityTimeoutDuration := EnvDuration("ASG_ACTIVITY_TIMEOUT", 10*time.Second)
	scaleInCooldownPeriod := EnvDuration("SCALE_IN_COOLDOWN_PERIOD", 0)
	scaleInFactor := math.Abs(EnvFloat("SCALE_IN_FACTOR"))
	scaleOutCooldownPeriod := EnvDuration("SCALE_OUT_COOLDOWN_PERIOD", 0)
	scaleOutFactor := math.Abs(EnvFloat("SCALE_OUT_FACTOR"))
	scaleOnlyAfterAllEvent := EnvBool("SCALE_ONLY_AFTER_ALL_EVENT")
	includeWaiting := EnvBool("INCLUDE_WAITING")
	instanceBuffer := EnvInt("INSTANCE_BUFFER", 0)
	maxDescribeScalingActivitiesPages := EnvInt("MAX_DESCRIBE_SCALING_ACTIVITIES_PAGES", -1)

	publishCloudWatchMetrics := EnvBool("CLOUDWATCH_METRICS")
	if publishCloudWatchMetrics {
		log.Print("Publishing cloudwatch metrics")
	}

	disableScaleIn := EnvBool("DISABLE_SCALE_IN")
	if disableScaleIn {
		log.Print("Disabling scale-in 🙅🏼‍")
	}

	disableScaleOut := EnvBool("DISABLE_SCALE_OUT")
	if disableScaleOut {
		log.Print("Disabling scale-out 🙅🏼‍♂️")
	}

	// establish an AWS session to be re-used
	sess, err := session.NewSession()
	if err != nil {
		return "", err
	}

	// get last scale in and out from asg's activities
	// This is wrapped in a mutex to avoid multiple outbound requests if the
	// lambda ever runs multiple times in parallel.
	func() {
		lastScaleMu.Lock()
		defer lastScaleMu.Unlock()

		if lastScaleTimesFetched {
			// We've already fetched the last scaling times that we need.
			return
		}

		asg := &scaler.ASGDriver{
			Name:                              asgName,
			Sess:                              sess,
			MaxDescribeScalingActivitiesPages: maxDescribeScalingActivitiesPages,
		}

		cctx, cancel := context.WithTimeout(ctx, asgActivityTimeoutDuration)
		defer cancel()

		scalingLastActivityStartTime := time.Now()
		scaleOutOutput, scaleInOutput, err := asg.GetLastScalingInAndOutActivity(cctx, !disableScaleOut, !disableScaleIn)
		if errors.Is(err, context.DeadlineExceeded) {
			log.Printf("Failed to retrieve last scaling activity events due to %v timeout", asgActivityTimeoutDuration)
			return
		}
		if err != nil { // Some other error.
			log.Printf("Encountered error when retrieving last scaling activities: %s", err)
			return
		}

		lastScaleInStr := "never"
		if scaleInOutput != nil && scaleInOutput.StartTime != nil {
			lastScaleIn = *scaleInOutput.StartTime
			lastScaleInStr = lastScaleIn.Format(time.RFC3339Nano)
		}
		lastScaleOutStr := "never"
		if scaleOutOutput != nil && scaleOutOutput.StartTime != nil {
			lastScaleOut = *scaleOutOutput.StartTime
			lastScaleOutStr = lastScaleOut.Format(time.RFC3339Nano)
		}

		lastScaleTimesFetched = true

		scalingTimeDiff := time.Since(scalingLastActivityStartTime)
		log.Printf("Succesfully retrieved last scaling activity events. Last scale out %s, last scale in %s. Discovery took %s.", lastScaleOutStr, lastScaleInStr, scalingTimeDiff)
	}()

	token := os.Getenv("BUILDKITE_AGENT_TOKEN")
	ssmTokenKey := os.Getenv("BUILDKITE_AGENT_TOKEN_SSM_KEY")

	if ssmTokenKey != "" {
		tk, err := scaler.RetrieveFromParameterStore(sess, ssmTokenKey)
		if err != nil {
			return "", err
		}
		token = tk
	}

	if token == "" {
		return "", errors.New("Must provide either BUILDKITE_AGENT_TOKEN or BUILDKITE_AGENT_TOKEN_SSM_KEY")
	}

	client := buildkite.NewClient(token)
	params := scaler.Params{
		BuildkiteQueue:       buildkiteQueue,
		AutoScalingGroupName: asgName,
		AgentsPerInstance:    agentsPerInstance,
		IncludeWaiting:       includeWaiting,
		ScaleInParams: scaler.ScaleParams{
			CooldownPeriod: scaleInCooldownPeriod,
			Factor:         scaleInFactor,
			LastEvent:      lastScaleIn,
			Disable:        disableScaleIn,
		},
		ScaleOutParams: scaler.ScaleParams{
			CooldownPeriod: scaleOutCooldownPeriod,
			Factor:         scaleOutFactor,
			LastEvent:      lastScaleOut,
			Disable:        disableScaleOut,
		},
		InstanceBuffer:           instanceBuffer,
		ScaleOnlyAfterAllEvent:   scaleOnlyAfterAllEvent,
		PublishCloudWatchMetrics: publishCloudWatchMetrics,
	}

	scaler, err := scaler.NewScaler(client, sess, params)
	if err != nil {
		log.Fatalf("Couldn't create new scaler: %v", err)
	}

	for {
		minPollDuration, err := scaler.Run()
		if err != nil {
			log.Printf("Scaling error: %v", err)
		}

		if interval < minPollDuration {
			interval = minPollDuration
			log.Printf("Increasing poll interval to %v based on rate limit",
				interval)
		}

		// Persist the times back into the global state
		lastScaleIn = scaler.LastScaleIn()
		lastScaleOut = scaler.LastScaleOut()

		logMsg := "Waiting for LAMBDA_INTERVAL (%v)"
		if timeout != nil {
			logMsg += " or timeout"
		}
		log.Printf(logMsg, interval)
		select {
		case <-timeout:
			log.Printf("Exiting due to LAMBDA_TIMEOUT (%v)", timeoutDuration)
			return "", nil

		case <-time.After(interval):
			// Continue
		}
	}
}
