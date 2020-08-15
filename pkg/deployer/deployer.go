package deployer

import (
	"errors"
	"fmt"
	"github.com/DevopsArtFactory/goployer/pkg/aws"
	"github.com/DevopsArtFactory/goployer/pkg/builder"
	"github.com/DevopsArtFactory/goployer/pkg/collector"
	"github.com/DevopsArtFactory/goployer/pkg/schemas"
	"github.com/DevopsArtFactory/goployer/pkg/slack"
	"github.com/DevopsArtFactory/goployer/pkg/tool"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	Logger "github.com/sirupsen/logrus"
	"time"
)

// Deployer per stack
type Deployer struct {
	Mode              string
	AsgNames          map[string]string
	PrevAsgs          map[string][]string
	PrevInstances     map[string][]string
	PrevVersions      map[string][]int
	PrevInstanceCount map[string]schemas.Capacity
	Logger            *Logger.Logger
	Stack             schemas.Stack
	AwsConfig         schemas.AWSConfig
	AWSClients        []aws.AWSClient
	LocalProvider     builder.UserdataProvider
	Slack             slack.Slack
	Collector         collector.Collector
	StepStatus        map[int64]bool
}

// getCurrentVersion returns current version for current deployment step
func getCurrentVersion(prevVersions []int) int {
	if len(prevVersions) == 0 {
		return 0
	}
	return (prevVersions[len(prevVersions)-1] + 1) % 1000
}

// Polling for healthcheck
func (d Deployer) polling(region schemas.RegionConfig, asg *autoscaling.Group, client aws.AWSClient, forceManifestCapacity, isUpdate, downsizingUpdate bool) (bool, error) {
	if *asg.AutoScalingGroupName == "" {
		return false, fmt.Errorf("no autoscaling found for %s", d.AsgNames[region.Region])
	}

	var threshold int64
	if !forceManifestCapacity && d.PrevInstanceCount[region.Region].Desired > d.Stack.Capacity.Desired {
		threshold = d.PrevInstanceCount[region.Region].Desired
	} else {
		threshold = d.Stack.Capacity.Desired
	}

	if region.HealthcheckTargetGroup == "" && region.HealthcheckLB == "" {
		d.Logger.Infof("healthcheck skipped because of neither target group nor classic load balancer specified")
		return true, nil
	}

	var targetHosts []aws.HealthcheckHost
	var err error
	validHostCount := int64(0)
	if region.HealthcheckTargetGroup != "" {
		var healthcheckTargetGroupArn *string
		if tool.IsTargetGroupArn(region.HealthcheckTargetGroup, region.Region) {
			healthcheckTargetGroupArn = &region.HealthcheckTargetGroup
		} else {
			tgs := []string{region.HealthcheckTargetGroup}
			tgArns, err := client.ELBV2Service.GetTargetGroupARNs(tgs)
			if err != nil {
				fmt.Println(err)
				return false, err
			}
			healthcheckTargetGroupArn = tgArns[0]
		}

		d.Logger.Debugf("[Checking healthy host count] Autoscaling Group: %s", *asg.AutoScalingGroupName)
		targetHosts, err = client.ELBV2Service.GetHostInTarget(asg, healthcheckTargetGroupArn, isUpdate, downsizingUpdate)
		if err != nil {
			return false, err
		}
	} else if region.HealthcheckLB != "" {
		d.Logger.Debugf("[Checking healthy host count] Autoscaling Group: %s", *asg.AutoScalingGroupName)
		targetHosts, err = client.ELBService.GetHealthyHostInELB(asg, region.HealthcheckLB)
		if err != nil {
			return false, err
		}
	}
	validHostCount = getValidHostCount(targetHosts)

	if isUpdate {
		if validHostCount == threshold {
			d.Logger.Info(fmt.Sprintf("[Update completed] current / desired : %d/%d", validHostCount, threshold))
			return true, nil
		}
		d.Logger.Info(fmt.Sprintf("Desired count does not meet the requirement: %d/%d", validHostCount, threshold))

	} else {
		if validHostCount >= threshold {
			d.Logger.Info(fmt.Sprintf("Healthy Count for %s : %d/%d", d.AsgNames[region.Region], validHostCount, threshold))
			d.Slack.SendSimpleMessage(fmt.Sprintf("All instances are healthy in %s  :  %d/%d", d.AsgNames[region.Region], validHostCount, threshold), d.Stack.Env)
			return true, nil
		}

		d.Logger.Info(fmt.Sprintf("Healthy count does not meet the requirement(%s) : %d/%d", d.AsgNames[region.Region], validHostCount, threshold))
		d.Slack.SendSimpleMessage(fmt.Sprintf("Waiting for healthy instances %s  :  %d/%d", d.AsgNames[region.Region], validHostCount, threshold), d.Stack.Env)
	}
	return false, nil
}

// CheckTerminating checks if all of instances are terminated well
func (d Deployer) CheckTerminating(client aws.AWSClient, target string, disableMetrics bool) bool {
	asgInfo, err := client.EC2Service.GetMatchingAutoscalingGroup(target)
	if err != nil {
		d.Logger.Errorf(err.Error())
		return true
	}

	if asgInfo == nil {
		d.Logger.Info("Already deleted autoscaling group : ", target)
		return true
	}

	d.Logger.Info(fmt.Sprintf("Waiting for instance termination in asg %s", target))
	if len(asgInfo.Instances) > 0 {
		d.Logger.Info(fmt.Sprintf("%d instance found : %s", len(asgInfo.Instances), target))
		d.Slack.SendSimpleMessage(fmt.Sprintf("Still %d instance found : %s", len(asgInfo.Instances), target), d.Stack.Env)

		return false
	}
	d.Slack.SendSimpleMessage(fmt.Sprintf(":+1: All instances are deleted : %s", target), d.Stack.Env)

	if err := d.CleanAutoscalingSet(client, target); err != nil {
		d.Logger.Errorf(err.Error())
		return false
	}

	if !disableMetrics {
		d.Logger.Debugf("update status of autoscaling group to teminated : %s", target)
		if err := d.Collector.UpdateStatus(target, "terminated", nil); err != nil {
			d.Logger.Errorf(err.Error())
			return false
		}
		d.Logger.Debugf("update status of %s is finished", target)
	}

	d.Logger.Debug(fmt.Sprintf("Start deleting launch templates in %s", target))
	if err := client.EC2Service.DeleteLaunchTemplates(target); err != nil {
		d.Logger.Errorln(err.Error())
		return false
	}
	d.Logger.Debug(fmt.Sprintf("Launch templates are deleted in %s\n", target))

	return true
}

func (d Deployer) CleanAutoscalingSet(client aws.AWSClient, target string) error {
	d.Logger.Debug(fmt.Sprintf("Start deleting autoscaling group : %s", target))
	if err := client.EC2Service.DeleteAutoscalingSet(target); err != nil {
		return err
	}
	d.Logger.Debug(fmt.Sprintf("Autoscaling group is deleted : %s", target))

	return nil
}

// ResizingAutoScalingGroupToZero set autoscaling group instance count to 0
func (d Deployer) ResizingAutoScalingGroupToZero(client aws.AWSClient, stack, asg string) error {
	d.Logger.Info(fmt.Sprintf("Modifying the size of autoscaling group to 0 : %s(%s)", asg, stack))
	d.Slack.SendSimpleMessage(fmt.Sprintf("Modifying the size of autoscaling group to 0 : %s/%s", asg, stack), d.Stack.Env)

	retry := int64(3)
	var err error
	for {
		err, retry = client.EC2Service.UpdateAutoScalingGroupSize(asg, 0, 0, 0, retry)
		if err != nil {
			if retry > 0 {
				d.Logger.Debugf("error occurred and remained retry count is %d", retry)
				time.Sleep(time.Duration(1+(2-retry)) * time.Second)
			} else {
				return err
			}
		}

		if err == nil {
			break
		}
	}

	return nil
}

// RunLifecycleCallbacks runs commands before terminating.
func (d Deployer) RunLifecycleCallbacks(client aws.AWSClient, target []string) bool {

	if len(target) == 0 {
		d.Logger.Debugf("no target instance exists\n")
		return false
	}

	commands := []string{}

	for _, command := range d.Stack.LifecycleCallbacks.PreTerminatePastClusters {
		commands = append(commands, command)
	}

	d.Logger.Debugf("run lifecycle callbacks before termination : %s", target)
	client.SSMService.SendCommand(
		aws.MakeStringArrayToAwsStrings(target),
		aws.MakeStringArrayToAwsStrings(commands),
	)

	return false
}

// selectClientFromList get aws client.
func selectClientFromList(awsClients []aws.AWSClient, region string) (aws.AWSClient, error) {
	for _, c := range awsClients {
		if c.Region == region {
			return c, nil
		}
	}
	return aws.AWSClient{}, errors.New("no AWS Client is selected")
}

// CheckTerminating checks if all of instances are terminated well
func (d Deployer) GatherMetrics(client aws.AWSClient, target string) error {
	targetGroups, err := client.EC2Service.GetTargetGroups(target)
	if err != nil {
		return err
	}

	if len(targetGroups) == 0 {
		d.Logger.Warnf("this autoscaling group does not belong to any target group ")
		return nil
	}

	d.Logger.Debugf("start retrieving additional metrics")
	metricData, err := d.Collector.GetAdditionalMetric(target, targetGroups, d.Logger)
	if err != nil {
		return err
	}

	d.Logger.Debugf("start updating additional metrics to DynamoDB")
	if err := d.Collector.UpdateStatistics(target, metricData); err != nil {
		return err
	}
	d.Logger.Debugf("finish updating additional metrics to DynamoDB")

	return nil
}

// getValidHostCount return the number of health host
func getValidHostCount(targetHosts []aws.HealthcheckHost) int64 {
	ret := 0
	for _, host := range targetHosts {
		Logger.Info(fmt.Sprintf("%+v", host))
		if host.Valid {
			ret += 1
		}
	}

	return int64(ret)
}
