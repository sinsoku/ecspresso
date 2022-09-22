package ecspresso

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/kayac/ecspresso/appspec"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/codedeploy"
	cdTypes "github.com/aws/aws-sdk-go-v2/service/codedeploy/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
	isatty "github.com/mattn/go-isatty"
	"github.com/pkg/errors"
)

const (
	CodeDeployConsoleURLFmt = "https://%s.console.aws.amazon.com/codesuite/codedeploy/deployments/%s?region=%s"
)

func calcDesiredCount(sv *Service, opt optWithDesiredCount) *int32 {
	if sv.SchedulingStrategy == types.SchedulingStrategyDaemon {
		return nil
	}
	if oc := opt.getDesiredCount(); oc != nil {
		if *oc == DefaultDesiredCount {
			return sv.DesiredCount
		}
		return oc // --tasks
	}
	return nil
}

func (d *App) Deploy(opt DeployOption) error {
	ctx, cancel := d.Start()
	defer cancel()

	var sv *Service
	d.Log("Starting deploy", opt.DryRunString())
	sv, err := d.DescribeServiceStatus(ctx, 0)
	if err != nil {
		return errors.Wrap(err, "failed to describe current service status")
	}

	var tdArn string
	if *opt.LatestTaskDefinition {
		family := strings.Split(arnToName(*sv.TaskDefinition), ":")[0]
		var err error
		tdArn, err = d.findLatestTaskDefinitionArn(ctx, family)
		if err != nil {
			return errors.Wrap(err, "failed to load latest task definition")
		}
	} else if *opt.SkipTaskDefinition {
		tdArn = *sv.TaskDefinition
	} else {
		td, err := d.LoadTaskDefinition(d.config.TaskDefinitionPath)
		if err != nil {
			return errors.Wrap(err, "failed to load task definition")
		}
		if *opt.DryRun {
			d.Log("task definition:")
			d.LogJSON(td)
		} else {
			newTd, err := d.RegisterTaskDefinition(ctx, td)
			if err != nil {
				return errors.Wrap(err, "failed to register task definition")
			}
			tdArn = *newTd.TaskDefinitionArn
		}
	}

	var count *int32
	if d.config.ServiceDefinitionPath != "" && aws.ToBool(opt.UpdateService) {
		newSv, err := d.LoadServiceDefinition(d.config.ServiceDefinitionPath)
		if err != nil {
			return errors.Wrap(err, "failed to load service definition")
		}
		ds, err := diffServices(newSv, sv, "", d.config.ServiceDefinitionPath, true)
		if err != nil {
			return errors.Wrap(err, "failed to diff of service definitions")
		}
		if ds != "" {
			if err = d.UpdateServiceAttributes(ctx, newSv, opt); err != nil {
				return errors.Wrap(err, "failed to update service attributes")
			}
			sv = newSv // updated
		} else {
			d.Log("service attributes will not change")
		}
		count = calcDesiredCount(newSv, opt)
	} else {
		count = calcDesiredCount(sv, opt)
	}
	if count != nil {
		d.Log("desired count:", *count)
	} else {
		d.Log("desired count: unchanged")
	}

	if *opt.DryRun {
		d.Log("DRY RUN OK")
		return nil
	}

	// manage auto scaling only when set option --suspend-auto-scaling or --no-suspend-auto-scaling explicitly
	if suspendState := opt.SuspendAutoScaling; suspendState != nil {
		if err := d.suspendAutoScaling(ctx, *suspendState); err != nil {
			return err
		}
	}

	// detect controller
	if dc := sv.DeploymentController; dc != nil {
		switch dc.Type {
		case types.DeploymentControllerTypeCodeDeploy:
			return d.DeployByCodeDeploy(ctx, tdArn, count, sv, opt)
		case types.DeploymentControllerTypeEcs:
			break
		default:
			return fmt.Errorf("could not deploy a service using deployment controller type %s", dc.Type)
		}
	}

	// rolling deploy (ECS internal)
	if err := d.UpdateServiceTasks(ctx, tdArn, count, opt); err != nil {
		return errors.Wrap(err, "failed to update service tasks")
	}

	if *opt.NoWait {
		d.Log("Service is deployed.")
		return nil
	}

	if err := d.WaitServiceStable(ctx, time.Now()); err != nil {
		return errors.Wrap(err, "failed to wait service stable")
	}

	d.Log("Service is stable now. Completed!")
	return nil
}

func (d *App) UpdateServiceTasks(ctx context.Context, taskDefinitionArn string, count *int32, opt DeployOption) error {
	in := &ecs.UpdateServiceInput{
		Service:            aws.String(d.Service),
		Cluster:            aws.String(d.Cluster),
		TaskDefinition:     aws.String(taskDefinitionArn),
		DesiredCount:       count,
		ForceNewDeployment: *opt.ForceNewDeployment,
	}
	msg := "Updating service tasks"
	if *opt.ForceNewDeployment {
		msg = msg + " with force new deployment"
	}
	msg = msg + "..."
	d.Log(msg)
	d.LogJSON(in)

	_, err := d.ecs.UpdateService(ctx, in)
	if err != nil {
		return err
	}
	time.Sleep(delayForServiceChanged) // wait for service updated
	return nil
}

func svToUpdateServiceInput(sv *Service) *ecs.UpdateServiceInput {
	in := &ecs.UpdateServiceInput{
		CapacityProviderStrategy:      sv.CapacityProviderStrategy,
		DeploymentConfiguration:       sv.DeploymentConfiguration,
		DesiredCount:                  sv.DesiredCount,
		EnableECSManagedTags:          &sv.EnableECSManagedTags,
		EnableExecuteCommand:          &sv.EnableExecuteCommand,
		HealthCheckGracePeriodSeconds: sv.HealthCheckGracePeriodSeconds,
		LoadBalancers:                 sv.LoadBalancers,
		NetworkConfiguration:          sv.NetworkConfiguration,
		PlacementConstraints:          sv.PlacementConstraints,
		PlacementStrategy:             sv.PlacementStrategy,
		PlatformVersion:               sv.PlatformVersion,
		PropagateTags:                 sv.PropagateTags,
		ServiceRegistries:             sv.ServiceRegistries,
	}
	if sv.SchedulingStrategy == types.SchedulingStrategyDaemon {
		in.PlacementStrategy = nil
	}
	return in
}

func (d *App) UpdateServiceAttributes(ctx context.Context, sv *Service, opt DeployOption) error {
	in := svToUpdateServiceInput(sv)
	if sv.DeploymentController != nil && sv.DeploymentController.Type == types.DeploymentControllerTypeCodeDeploy {
		// unable to update attributes below with a CODE_DEPLOY deployment controller.
		in.NetworkConfiguration = nil
		in.PlatformVersion = nil
		in.ForceNewDeployment = false
		in.LoadBalancers = nil
		in.ServiceRegistries = nil
	} else {
		in.ForceNewDeployment = aws.ToBool(opt.ForceNewDeployment)
	}
	in.Service = aws.String(d.Service)
	in.Cluster = aws.String(d.Cluster)

	if *opt.DryRun {
		d.Log("update service input:")
		d.LogJSON(in)
		return nil
	}
	d.Log("Updating service attributes...")
	d.DebugLog(in)

	if _, err := d.ecs.UpdateService(ctx, in); err != nil {
		return err
	}
	time.Sleep(delayForServiceChanged) // wait for service updated
	return nil
}

func (d *App) DeployByCodeDeploy(ctx context.Context, taskDefinitionArn string, count *int32, sv *Service, opt DeployOption) error {
	if count != nil {
		d.Log("updating desired count to", *count)
	}
	_, err := d.ecs.UpdateService(
		ctx,
		&ecs.UpdateServiceInput{
			Service:      aws.String(d.Service),
			Cluster:      aws.String(d.Cluster),
			DesiredCount: count,
		},
	)
	if err != nil {
		return errors.Wrap(err, "failed to update service")
	}
	if aws.ToBool(opt.SkipTaskDefinition) && !aws.ToBool(opt.UpdateService) && !aws.ToBool(opt.ForceNewDeployment) {
		// no need to create new deployment.
		return nil
	}

	return d.createDeployment(ctx, sv, taskDefinitionArn, opt.RollbackEvents)
}

func (d *App) findDeploymentInfo(ctx context.Context) (*cdTypes.DeploymentInfo, error) {
	// search deploymentGroup in CodeDeploy
	d.DebugLog("find all applications in CodeDeploy")
	la, err := d.codedeploy.ListApplications(ctx, &codedeploy.ListApplicationsInput{})
	if err != nil {
		return nil, err
	}
	if len(la.Applications) == 0 {
		return nil, errors.New("no any applications in CodeDeploy")
	}
	// BatchGetApplications accepts applications less than 100
	for i := 0; i < len(la.Applications); i += 100 {
		end := i + 100
		if end > len(la.Applications) {
			end = len(la.Applications)
		}
		apps, err := d.codedeploy.BatchGetApplications(ctx, &codedeploy.BatchGetApplicationsInput{
			ApplicationNames: la.Applications[i:end],
		})
		if err != nil {
			return nil, err
		}
		for _, info := range apps.ApplicationsInfo {
			d.DebugLog("application", info)
			if info.ComputePlatform != cdTypes.ComputePlatformEcs {
				continue
			}
			lg, err := d.codedeploy.ListDeploymentGroups(ctx, &codedeploy.ListDeploymentGroupsInput{
				ApplicationName: info.ApplicationName,
			})
			if err != nil {
				return nil, err
			}
			if len(lg.DeploymentGroups) == 0 {
				d.DebugLog("no deploymentGroups in application", *info.ApplicationName)
				continue
			}
			groups, err := d.codedeploy.BatchGetDeploymentGroups(ctx, &codedeploy.BatchGetDeploymentGroupsInput{
				ApplicationName:      info.ApplicationName,
				DeploymentGroupNames: lg.DeploymentGroups,
			})
			if err != nil {
				return nil, err
			}
			for _, dg := range groups.DeploymentGroupsInfo {
				d.DebugLog("deploymentGroup", dg)
				for _, ecsService := range dg.EcsServices {
					if *ecsService.ClusterName == d.config.Cluster && *ecsService.ServiceName == d.config.Service {
						return &cdTypes.DeploymentInfo{
							ApplicationName:      aws.String(*info.ApplicationName),
							DeploymentGroupName:  aws.String(*dg.DeploymentGroupName),
							DeploymentConfigName: aws.String(*dg.DeploymentConfigName),
						}, nil
					}
				}
			}
		}
	}
	return nil, fmt.Errorf(
		"failed to find CodeDeploy Application/DeploymentGroup for ECS service %s on cluster %s",
		d.config.Service,
		d.config.Cluster,
	)
}

func (d *App) createDeployment(ctx context.Context, sv *Service, taskDefinitionArn string, rollbackEvents *string) error {

	spec, err := appspec.NewWithService(&sv.Service, taskDefinitionArn)
	if err != nil {
		return errors.Wrap(err, "failed to create appspec")
	}
	if d.config.AppSpec != nil {
		spec.Hooks = d.config.AppSpec.Hooks
	}
	d.DebugLog("appSpecContent:", spec.String())

	// deployment
	dp, err := d.findDeploymentInfo(ctx)
	if err != nil {
		return err
	}
	dd := &codedeploy.CreateDeploymentInput{
		ApplicationName:      dp.ApplicationName,
		DeploymentGroupName:  dp.DeploymentGroupName,
		DeploymentConfigName: dp.DeploymentConfigName,
		Revision: &cdTypes.RevisionLocation{
			RevisionType: cdTypes.RevisionLocationTypeAppSpecContent,
			AppSpecContent: &cdTypes.AppSpecContent{
				Content: aws.String(spec.String()),
			},
		},
	}
	if ev := aws.ToString(rollbackEvents); ev != "" {
		var events []cdTypes.AutoRollbackEvent
		for _, ev := range strings.Split(ev, ",") {
			switch ev {
			case "DEPLOYMENT_FAILURE":
				events = append(events, cdTypes.AutoRollbackEventDeploymentFailure)
			case "DEPLOYMENT_STOP_ON_ALARM":
				events = append(events, cdTypes.AutoRollbackEventDeploymentStopOnAlarm)
			case "DEPLOYMENT_STOP_ON_REQUEST":
				events = append(events, cdTypes.AutoRollbackEventDeploymentStopOnRequest)
			default:
				return fmt.Errorf("invalid rollback event: %s", ev)
			}
		}
		dd.AutoRollbackConfiguration = &cdTypes.AutoRollbackConfiguration{
			Enabled: true,
			Events:  events,
		}
	}

	d.DebugLog("creating a deployment to CodeDeploy", dd)

	res, err := d.codedeploy.CreateDeployment(ctx, dd)
	if err != nil {
		return errors.Wrap(err, "failed to create deployment")
	}
	id := *res.DeploymentId
	u := fmt.Sprintf(
		CodeDeployConsoleURLFmt,
		d.config.Region,
		id,
		d.config.Region,
	)
	d.Log(fmt.Sprintf("Deployment %s is created on CodeDeploy:", id))
	d.Log(u)

	if isatty.IsTerminal(os.Stdout.Fd()) {
		if err := exec.Command("open", u).Start(); err != nil {
			d.Log("Couldn't open URL", u)
		}
	}
	return nil
}
