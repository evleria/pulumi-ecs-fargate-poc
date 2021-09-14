package main

import (
	"encoding/base64"
	"fmt"
	"github.com/pulumi/pulumi-aws/sdk/v4/go/aws/ec2"
	"github.com/pulumi/pulumi-aws/sdk/v4/go/aws/ecr"
	"github.com/pulumi/pulumi-aws/sdk/v4/go/aws/ecs"
	elb "github.com/pulumi/pulumi-aws/sdk/v4/go/aws/elasticloadbalancingv2"
	"github.com/pulumi/pulumi-aws/sdk/v4/go/aws/iam"
	"github.com/pulumi/pulumi-docker/sdk/v3/go/docker"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"strings"
)

const (
	SecurityGroupName  = "web-sg"
	EcsClusterName     = "app-cluster"
	TaskExecRoleName   = "task-exec-role"
	TaskExecPolicyName = "task-exec-policy"
	EcrRepositoryName  = "app-repo"
	EcsTaskName        = "app-task"
	EcsServiceName     = "app-service"
	ContainerName      = "my-app"
	ImageName          = "my-image"

	LoadBalancerName            = "web-lb"
	LoadBalancerTargetGroupName = "web-tg"
	LoadBalancerListenerName    = "web-listener"

	ReplicasCount = 5
)

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		vpc, err := getDefaultVPC(ctx)
		if err != nil {
			return err
		}

		securityGroup, err := newSecurityGroup(ctx, vpc)
		if err != nil {
			return err
		}

		cluster, err := ecs.NewCluster(ctx, EcsClusterName, nil)
		if err != nil {
			return err
		}

		taskExecRole, err := newTaskExecRole(ctx)
		if err != nil {
			return err
		}

		subnet, loadBalancer, targetGroup, webListener, err := newLoadBalancer(ctx, vpc, securityGroup)
		if err != nil {
			return err
		}

		repo, err := ecr.NewRepository(ctx, EcrRepositoryName, &ecr.RepositoryArgs{})
		if err != nil {
			return err
		}

		containerDef, err := pushImageAndGetContainerDef(ctx, repo, "..")
		if err != nil {
			return err
		}

		appTask, err := ecs.NewTaskDefinition(ctx, EcsTaskName, &ecs.TaskDefinitionArgs{
			Family:                  pulumi.String("fargate-task-definition"),
			Cpu:                     pulumi.String("256"),
			Memory:                  pulumi.String("512"),
			NetworkMode:             pulumi.String("awsvpc"),
			RequiresCompatibilities: pulumi.StringArray{pulumi.String("FARGATE")},
			ExecutionRoleArn:        taskExecRole.Arn,
			ContainerDefinitions:    containerDef,
		})
		if err != nil {
			return err
		}
		_, err = ecs.NewService(ctx, EcsServiceName, &ecs.ServiceArgs{
			Cluster:        cluster.Arn,
			DesiredCount:   pulumi.Int(ReplicasCount),
			LaunchType:     pulumi.String("FARGATE"),
			TaskDefinition: appTask.Arn,
			NetworkConfiguration: &ecs.ServiceNetworkConfigurationArgs{
				AssignPublicIp: pulumi.Bool(true),
				Subnets:        toPulumiStringArray(subnet.Ids),
				SecurityGroups: pulumi.StringArray{securityGroup.ID().ToStringOutput()},
			},
			LoadBalancers: ecs.ServiceLoadBalancerArray{
				ecs.ServiceLoadBalancerArgs{
					TargetGroupArn: targetGroup.Arn,
					ContainerName:  pulumi.String(ContainerName),
					ContainerPort:  pulumi.Int(80),
				},
			},
		}, pulumi.DependsOn([]pulumi.Resource{webListener}))

		ctx.Export("url", loadBalancer.DnsName)
		return nil
	})
}

func pushImageAndGetContainerDef(ctx *pulumi.Context, repo *ecr.Repository, dockerFilePath string) (pulumi.StringOutput, error) {
	repoCreds := repo.RegistryId.ApplyT(func(rid string) ([]string, error) {
		creds, err := ecr.GetCredentials(ctx, &ecr.GetCredentialsArgs{
			RegistryId: rid,
		})
		if err != nil {
			return nil, err
		}
		data, err := base64.StdEncoding.DecodeString(creds.AuthorizationToken)
		if err != nil {
			return nil, err
		}

		return strings.Split(string(data), ":"), nil
	}).(pulumi.StringArrayOutput)
	repoUser := repoCreds.Index(pulumi.Int(0))
	repoPass := repoCreds.Index(pulumi.Int(1))

	image, err := docker.NewImage(ctx, ImageName, &docker.ImageArgs{
		Build: docker.DockerBuildArgs{
			Context: pulumi.String(dockerFilePath),
		},
		ImageName: repo.RepositoryUrl,
		Registry: docker.ImageRegistryArgs{
			Server:   repo.RepositoryUrl,
			Username: repoUser,
			Password: repoPass,
		},
	})

	if err != nil {
		return pulumi.StringOutput{}, err
	}

	containerDef := image.ImageName.ApplyT(func(name string) (string, error) {
		fmtstr := `[{
				"name": "my-app",
				"image": %q,
				"portMappings": [{
					"containerPort": 80,
					"hostPort": 80,
					"protocol": "tcp"
				}]
			}]`
		return fmt.Sprintf(fmtstr, name), nil
	}).(pulumi.StringOutput)
	return containerDef, nil
}

func newLoadBalancer(ctx *pulumi.Context, vpc *ec2.LookupVpcResult, webSg *ec2.SecurityGroup) (*ec2.GetSubnetIdsResult, *elb.LoadBalancer, *elb.TargetGroup, *elb.Listener, error) {
	subnet, err := ec2.GetSubnetIds(ctx, &ec2.GetSubnetIdsArgs{VpcId: vpc.Id})
	if err != nil {
		return nil, nil, nil, nil, err
	}

	webLb, err := elb.NewLoadBalancer(ctx, LoadBalancerName, &elb.LoadBalancerArgs{
		Subnets:        toPulumiStringArray(subnet.Ids),
		SecurityGroups: pulumi.StringArray{webSg.ID().ToStringOutput()},
	})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	webTg, err := elb.NewTargetGroup(ctx, LoadBalancerTargetGroupName, &elb.TargetGroupArgs{
		Port:       pulumi.Int(80),
		Protocol:   pulumi.String("HTTP"),
		TargetType: pulumi.String("ip"),
		VpcId:      pulumi.String(vpc.Id),
	})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	webListener, err := elb.NewListener(ctx, LoadBalancerListenerName, &elb.ListenerArgs{
		LoadBalancerArn: webLb.Arn,
		Port:            pulumi.Int(80),
		DefaultActions: elb.ListenerDefaultActionArray{
			elb.ListenerDefaultActionArgs{
				Type:           pulumi.String("forward"),
				TargetGroupArn: webTg.Arn,
			},
		},
	})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return subnet, webLb, webTg, webListener, nil
}

func newTaskExecRole(ctx *pulumi.Context) (*iam.Role, error) {
	taskExecRole, err := iam.NewRole(ctx, TaskExecRoleName, &iam.RoleArgs{
		AssumeRolePolicy: pulumi.String(`{
    "Version": "2008-10-17",
    "Statement": [{
        "Sid": "",
        "Effect": "Allow",
        "Principal": {
            "Service": "ecs-tasks.amazonaws.com"
        },
        "Action": "sts:AssumeRole"
    }]
}`),
	})
	if err != nil {
		return nil, err
	}

	_, err = iam.NewRolePolicyAttachment(ctx, TaskExecPolicyName, &iam.RolePolicyAttachmentArgs{
		Role:      taskExecRole.Name,
		PolicyArn: pulumi.String("arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"),
	})
	if err != nil {
		return nil, err
	}

	return taskExecRole, nil
}

func newSecurityGroup(ctx *pulumi.Context, vpc *ec2.LookupVpcResult) (*ec2.SecurityGroup, error) {
	return ec2.NewSecurityGroup(ctx, SecurityGroupName, &ec2.SecurityGroupArgs{
		VpcId:  pulumi.String(vpc.Id),
		Egress: getUnrestrictedEgress(),
		Ingress: ec2.SecurityGroupIngressArray{
			ec2.SecurityGroupIngressArgs{
				Protocol:   pulumi.String("tcp"),
				FromPort:   pulumi.Int(80),
				ToPort:     pulumi.Int(80),
				CidrBlocks: pulumi.StringArray{pulumi.String("0.0.0.0/0")},
			},
		},
	})
}

func getUnrestrictedEgress() ec2.SecurityGroupEgressArrayInput {
	return ec2.SecurityGroupEgressArray{
		ec2.SecurityGroupEgressArgs{
			Protocol:   pulumi.String("-1"),
			FromPort:   pulumi.Int(0),
			ToPort:     pulumi.Int(0),
			CidrBlocks: pulumi.StringArray{pulumi.String("0.0.0.0/0")},
		},
	}
}

func getDefaultVPC(ctx *pulumi.Context) (*ec2.LookupVpcResult, error) {
	t := true
	return ec2.LookupVpc(ctx, &ec2.LookupVpcArgs{Default: &t})
}

func toPulumiStringArray(a []string) pulumi.StringArrayInput {
	var res []pulumi.StringInput
	for _, s := range a {
		res = append(res, pulumi.String(s))
	}
	return pulumi.StringArray(res)
}
