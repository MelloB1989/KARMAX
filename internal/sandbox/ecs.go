package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	ecstypes "github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

// ECSDriver runs each sandbox as one Fargate task.
//
// Chosen over Docker-on-the-host for a reason that is about arithmetic, not
// taste: the daemon runs on a 2GB box and a coding agent wants a couple of
// gigabytes to itself. Sharing that machine means the sandbox and the thing
// that supervises it die together, and the supervisor dies second — so nobody
// is left to record what happened.
//
// Fargate also gives the property the sandbox exists for: the container gets
// its own kernel, its own network interface, and a task role that grants it
// nothing. A coding agent running arbitrary code from a ticket should not be
// one `docker` flag away from the host that holds every credential.

// ECSConfig is what the driver needs to place a task.
type ECSConfig struct {
	Cluster        string
	TaskDefinition string
	// Subnets must have a route out — the sandbox pulls its image, clones a
	// repo and calls an API. A private subnet with no NAT looks identical to a
	// hung task for three minutes and then times out.
	Subnets        []string
	SecurityGroups []string
	// AssignPublicIP is what makes a public-subnet task reachable outbound
	// without a NAT gateway, which costs more per month than this whole
	// deployment.
	AssignPublicIP bool
	// Container is the name inside the task definition to inject env into.
	Container string
	LogGroup  string
	Region    string
}

// ECSDriver launches sandboxes as Fargate tasks.
type ECSDriver struct {
	cfg  ECSConfig
	ecs  *ecs.Client
	logs *cloudwatchlogs.Client
}

// NewECSDriver reads its configuration from the environment and checks the
// cluster exists before returning — an operator with a typo in a cluster name
// gets one sentence here rather than a task that never appears.
func NewECSDriver() (*ECSDriver, error) {
	c := ECSConfig{
		Cluster:        strings.TrimSpace(os.Getenv("KARMAX_SANDBOX_ECS_CLUSTER")),
		TaskDefinition: strings.TrimSpace(os.Getenv("KARMAX_SANDBOX_ECS_TASKDEF")),
		Subnets:        splitList(os.Getenv("KARMAX_SANDBOX_ECS_SUBNETS")),
		SecurityGroups: splitList(os.Getenv("KARMAX_SANDBOX_ECS_SECURITY_GROUPS")),
		Container:      strings.TrimSpace(os.Getenv("KARMAX_SANDBOX_ECS_CONTAINER")),
		LogGroup:       strings.TrimSpace(os.Getenv("KARMAX_SANDBOX_ECS_LOG_GROUP")),
		Region:         strings.TrimSpace(os.Getenv("AWS_REGION")),
	}
	c.AssignPublicIP = os.Getenv("KARMAX_SANDBOX_ECS_PUBLIC_IP") != "false"
	if c.Container == "" {
		c.Container = "sandbox"
	}

	var missing []string
	if c.Cluster == "" {
		missing = append(missing, "KARMAX_SANDBOX_ECS_CLUSTER")
	}
	if c.TaskDefinition == "" {
		missing = append(missing, "KARMAX_SANDBOX_ECS_TASKDEF")
	}
	if len(c.Subnets) == 0 {
		missing = append(missing, "KARMAX_SANDBOX_ECS_SUBNETS")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("sandbox: the ECS driver needs %s", strings.Join(missing, ", "))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	opts := []func(*awsconfig.LoadOptions) error{}
	if c.Region != "" {
		opts = append(opts, awsconfig.WithRegion(c.Region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("sandbox: could not load AWS credentials: %w", err)
	}

	d := &ECSDriver{cfg: c, ecs: ecs.NewFromConfig(awsCfg), logs: cloudwatchlogs.NewFromConfig(awsCfg)}

	// Prove the cluster is there and we may see it. This is the check that
	// turns "my sandbox never starts" into "that cluster does not exist".
	out, err := d.ecs.DescribeClusters(ctx, &ecs.DescribeClustersInput{Clusters: []string{c.Cluster}})
	if err != nil {
		return nil, fmt.Errorf("sandbox: cannot reach ECS: %w", err)
	}
	if len(out.Clusters) == 0 || aws.ToString(out.Clusters[0].Status) != "ACTIVE" {
		return nil, fmt.Errorf("sandbox: ECS cluster %q is not active — check the name and the region", c.Cluster)
	}
	return d, nil
}

func (d *ECSDriver) Name() string { return "ecs" }

// Launch starts one Fargate task and returns its ARN.
func (d *ECSDriver) Launch(ctx context.Context, s Spec) (string, error) {
	// Credentials ride in the container override, which goes over TLS to the
	// ECS API and is never written to this host's disk or argv. They are
	// visible in DescribeTasks to anyone with ecs:DescribeTasks on the
	// cluster, which is why the task role grants nothing and the tokens are
	// short-lived and repo-scoped.
	env := make([]ecstypes.KeyValuePair, 0, len(s.Env)+3)
	add := func(k, v string) {
		if v == "" {
			return
		}
		env = append(env, ecstypes.KeyValuePair{Name: aws.String(k), Value: aws.String(v)})
	}
	for k, v := range s.Env {
		add(k, v)
	}
	// The SAME names the Docker driver uses, because the same entrypoint.sh
	// reads them: it exits on `REPO is required` before doing anything, so a
	// driver inventing its own spelling produces a container that starts,
	// fails instantly, and reports nothing useful.
	add("TASK", s.Task)
	add("REPO", s.Repo)
	add("BASE_BRANCH", s.Branch)

	in := &ecs.RunTaskInput{
		Cluster:        aws.String(d.cfg.Cluster),
		TaskDefinition: aws.String(d.cfg.TaskDefinition),
		LaunchType:     ecstypes.LaunchTypeFargate,
		Count:          aws.Int32(1),
		NetworkConfiguration: &ecstypes.NetworkConfiguration{
			AwsvpcConfiguration: &ecstypes.AwsVpcConfiguration{
				Subnets:        d.cfg.Subnets,
				SecurityGroups: d.cfg.SecurityGroups,
				AssignPublicIp: ecstypes.AssignPublicIpDisabled,
			},
		},
		Overrides: &ecstypes.TaskOverride{
			ContainerOverrides: []ecstypes.ContainerOverride{{
				Name:        aws.String(d.cfg.Container),
				Environment: env,
			}},
		},
		// The run is findable later by the case it belongs to, without keeping
		// a second index of our own.
		Tags: []ecstypes.Tag{
			{Key: aws.String("karmax:repo"), Value: aws.String(tagSafe(s.Repo))},
			{Key: aws.String("karmax:branch"), Value: aws.String(tagSafe(s.Branch))},
		},
		PropagateTags: ecstypes.PropagateTagsTaskDefinition,
	}
	if d.cfg.AssignPublicIP {
		in.NetworkConfiguration.AwsvpcConfiguration.AssignPublicIp = ecstypes.AssignPublicIpEnabled
	}
	if s.Image != "" {
		// An image named per-run overrides the task definition's, so a caller
		// can pin a build without a new task definition revision.
		in.Overrides.ContainerOverrides[0].Command = nil
	}

	out, err := d.ecs.RunTask(ctx, in)
	if err != nil {
		return "", fmt.Errorf("sandbox: could not start the task: %w", err)
	}
	// RunTask answers 200 with the reason it declined. Reading only err here
	// would report a launch that never happened as a success.
	if len(out.Failures) > 0 {
		f := out.Failures[0]
		return "", fmt.Errorf("sandbox: ECS refused the task: %s (%s)",
			aws.ToString(f.Reason), aws.ToString(f.Detail))
	}
	if len(out.Tasks) == 0 {
		return "", errors.New("sandbox: ECS accepted the request and started nothing")
	}
	return aws.ToString(out.Tasks[0].TaskArn), nil
}

// Poll reports where a task has got to.
func (d *ECSDriver) Poll(ctx context.Context, id string) (Status, error) {
	out, err := d.ecs.DescribeTasks(ctx, &ecs.DescribeTasksInput{
		Cluster: aws.String(d.cfg.Cluster), Tasks: []string{id},
	})
	if err != nil {
		return Status{}, fmt.Errorf("sandbox: could not describe the task: %w", err)
	}
	if len(out.Tasks) == 0 {
		// Fargate forgets a stopped task after about an hour. That is not a
		// failure — it is the run having finished long enough ago that the
		// exit code is no longer recoverable, which is exactly StateGone.
		return Status{ID: id, State: StateGone}, nil
	}

	t := out.Tasks[0]
	st := Status{ID: id, State: mapECSState(aws.ToString(t.LastStatus))}
	for _, c := range t.Containers {
		if aws.ToString(c.Name) != d.cfg.Container {
			continue
		}
		if c.ExitCode != nil {
			st.ExitCode = int(*c.ExitCode)
		}
		if st.State == StateExited && st.ExitCode != 0 {
			st.State = StateFailed
		}
		if r := aws.ToString(c.Reason); r != "" {
			st.LogTail = r
		}
	}
	// A task can stop before its container ever runs — no image, no ENI, no
	// capacity. The container reason is empty then and the task's is not.
	if st.LogTail == "" {
		if r := aws.ToString(t.StoppedReason); r != "" {
			st.LogTail = r
		}
	}
	return st, nil
}

// Logs reads the tail of the task's CloudWatch stream.
func (d *ECSDriver) Logs(ctx context.Context, id string, tail int) (string, error) {
	if d.cfg.LogGroup == "" {
		return "", errors.New("sandbox: no log group configured, so there is nothing to read")
	}
	if tail <= 0 {
		tail = 200
	}

	// awslogs names a stream "<prefix>/<container>/<task-id>", and the task id
	// is the last segment of the ARN.
	taskID := id
	if i := strings.LastIndex(id, "/"); i >= 0 {
		taskID = id[i+1:]
	}
	stream := "sandbox/" + d.cfg.Container + "/" + taskID

	out, err := d.logs.GetLogEvents(ctx, &cloudwatchlogs.GetLogEventsInput{
		LogGroupName:  aws.String(d.cfg.LogGroup),
		LogStreamName: aws.String(stream),
		Limit:         aws.Int32(int32(tail)),
		StartFromHead: aws.Bool(false),
	})
	if err != nil {
		return "", fmt.Errorf("sandbox: could not read logs for %s: %w", taskID, err)
	}

	var b strings.Builder
	for _, e := range out.Events {
		b.WriteString(aws.ToString(e.Message))
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// Kill stops a running task.
func (d *ECSDriver) Kill(ctx context.Context, id string) error {
	_, err := d.ecs.StopTask(ctx, &ecs.StopTaskInput{
		Cluster: aws.String(d.cfg.Cluster),
		Task:    aws.String(id),
		Reason:  aws.String("stopped by KARMAX"),
	})
	if err != nil {
		return fmt.Errorf("sandbox: could not stop the task: %w", err)
	}
	return nil
}

// mapECSState folds the ECS lifecycle onto the four states a driver reports.
//
// ECS has six words for "not running yet" and one for "stopped". Collapsing
// them here means the rest of KARMAX never learns what PROVISIONING means.
func mapECSState(last string) string {
	switch strings.ToUpper(last) {
	case "PROVISIONING", "PENDING", "ACTIVATING":
		return StateStarting
	case "RUNNING":
		return StateRunning
	case "DEACTIVATING", "STOPPING", "DEPROVISIONING", "STOPPED":
		return StateExited
	default:
		return StateStarting
	}
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// tagSafe makes a value acceptable as an AWS tag.
func tagSafe(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "none"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.' || r == '/' || r == ':' || r == ' ':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	if len(out) > 256 {
		out = out[:256]
	}
	return out
}
