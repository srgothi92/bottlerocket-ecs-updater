package main

import (
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSendCommand(t *testing.T) {
	// commandSuccessInstance indicates an instance for which the command should succeed
	// regardless of whether `waitError` is set.
	const commandSuccessInstance = "inst-success"
	cases := []struct {
		name          string
		sendOutput    *ssm.SendCommandOutput
		sendError     error
		expectedError string
		expectedOut   string
		waitError     error
		instances     []string
	}{
		{
			name: "send success",
			sendOutput: &ssm.SendCommandOutput{
				Command: &ssm.Command{CommandId: aws.String("id1")},
			},
			instances:   []string{"inst-id-1"},
			expectedOut: "id1",
		},
		{
			name:          "send fail",
			sendError:     errors.New("failed to send command"),
			expectedError: "send command failed",
			instances:     []string{"inst-id-1"},
		},
		{
			name:      "wait single failure",
			waitError: errors.New("exceeded max attempts"),
			sendOutput: &ssm.SendCommandOutput{
				Command: &ssm.Command{CommandId: aws.String("")},
			},
			expectedError: "too many failures while awaiting document execution",
			instances:     []string{"inst-id-1"},
		},
		{
			name:      "wait one succcess",
			waitError: errors.New("exceeded max attempts"),
			sendOutput: &ssm.SendCommandOutput{
				Command: &ssm.Command{CommandId: aws.String("id1")},
			},
			instances:   []string{"inst-id-1", "inst-id-2", commandSuccessInstance},
			expectedOut: "id1",
		},
		{
			name:      "wait fail all",
			waitError: errors.New("exceeded max attempts"),
			sendOutput: &ssm.SendCommandOutput{
				Command: &ssm.Command{CommandId: aws.String("id1")},
			},
			expectedError: "too many failures while awaiting document execution",
			instances:     []string{"inst-id-1", "inst-id-2", "inst-id-3"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mockSSM := MockSSM{
				SendCommandFn: func(input *ssm.SendCommandInput) (*ssm.SendCommandOutput, error) {
					assert.Equal(t, "test-doc", aws.StringValue(input.DocumentName))
					assert.Equal(t, "$DEFAULT", aws.StringValue(input.DocumentVersion))
					assert.Equal(t, aws.StringSlice(tc.instances), input.InstanceIds)
					return tc.sendOutput, tc.sendError
				},
				WaitUntilCommandExecutedWithContextFn: func(ctx aws.Context, input *ssm.GetCommandInvocationInput, opts ...request.WaiterOption) error {
					if aws.StringValue(input.InstanceId) == commandSuccessInstance {
						return nil
					}
					return tc.waitError
				},
			}
			u := updater{ssm: mockSSM}
			actual, err := u.sendCommand(tc.instances, "test-doc")
			if tc.expectedOut != "" {
				require.NoError(t, err)
				assert.EqualValues(t, tc.expectedOut, actual)
			} else if tc.sendError != nil {
				assert.ErrorIs(t, err, tc.sendError)
				assert.Contains(t, err.Error(), tc.expectedError)
			} else {
				assert.ErrorIs(t, err, tc.waitError)
				assert.Contains(t, err.Error(), tc.expectedError)
			}
		})
	}
}

func TestListContainerInstances(t *testing.T) {
	cases := []struct {
		name          string
		listOutput    *ecs.ListContainerInstancesOutput
		listOutput2   *ecs.ListContainerInstancesOutput
		listError     error
		expectedError string
		expectedOut   []*string
	}{
		{
			name: "with instances",
			listOutput: &ecs.ListContainerInstancesOutput{
				ContainerInstanceArns: []*string{
					aws.String("cont-inst-arn1"),
					aws.String("cont-inst-arn2"),
					aws.String("cont-inst-arn3")},
				NextToken: aws.String("token"),
			},
			listOutput2: &ecs.ListContainerInstancesOutput{
				ContainerInstanceArns: []*string{
					aws.String("cont-inst-arn4"),
					aws.String("cont-inst-arn5"),
					aws.String("cont-inst-arn6")},
				NextToken: nil,
			},
			expectedOut: []*string{
				aws.String("cont-inst-arn1"),
				aws.String("cont-inst-arn2"),
				aws.String("cont-inst-arn3"),
				aws.String("cont-inst-arn4"),
				aws.String("cont-inst-arn5"),
				aws.String("cont-inst-arn6")},
		},
		{
			name: "without instances",
			listOutput: &ecs.ListContainerInstancesOutput{
				ContainerInstanceArns: []*string{},
			},
			listOutput2: &ecs.ListContainerInstancesOutput{
				ContainerInstanceArns: []*string{},
			},
			expectedOut: []*string{},
		},
		{
			name:      "list fail",
			listError: errors.New("failed to list instances"),
			listOutput: &ecs.ListContainerInstancesOutput{
				ContainerInstanceArns: []*string{},
			},
			listOutput2: &ecs.ListContainerInstancesOutput{
				ContainerInstanceArns: []*string{},
			},
			expectedError: "failed to list container instances",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mockECS := MockECS{
				ListContainerInstancesPagesFn: func(input *ecs.ListContainerInstancesInput, fn func(*ecs.ListContainerInstancesOutput, bool) bool) error {
					assert.Equal(t, ecs.ContainerInstanceStatusActive, aws.StringValue(input.Status))
					fn(tc.listOutput, true)
					fn(tc.listOutput2, false)
					return tc.listError
				},
			}
			u := updater{ecs: mockECS}
			actual, err := u.listContainerInstances()
			if tc.expectedOut != nil {
				assert.EqualValues(t, tc.expectedOut, actual)
				assert.NoError(t, err)
			} else {
				assert.Empty(t, actual)
				assert.ErrorIs(t, err, tc.listError)
				assert.Contains(t, err.Error(), tc.expectedError)
			}
		})
	}
}

func TestFilterBottlerocketInstances(t *testing.T) {
	output := &ecs.DescribeContainerInstancesOutput{
		ContainerInstances: []*ecs.ContainerInstance{{
			// Bottlerocket with single attribute
			Attributes:           []*ecs.Attribute{{Name: aws.String("bottlerocket.variant")}},
			ContainerInstanceArn: aws.String("cont-inst-br1"),
			Ec2InstanceId:        aws.String("ec2-id-br1"),
		}, {
			// Bottlerocket with extra attribute
			Attributes: []*ecs.Attribute{
				{Name: aws.String("different-attribute")},
				{Name: aws.String("bottlerocket.variant")},
			},
			ContainerInstanceArn: aws.String("cont-inst-br2"),
			Ec2InstanceId:        aws.String("ec2-id-br2"),
		}, {
			// Not Bottlerocket, single attribute
			Attributes: []*ecs.Attribute{
				{Name: aws.String("different-attribute")},
			},
			ContainerInstanceArn: aws.String("cont-inst-not1"),
			Ec2InstanceId:        aws.String("ec2-id-not1"),
		}, {
			// Not Bottlerocket, no attribute
			ContainerInstanceArn: aws.String("cont-inst-not2"),
			Ec2InstanceId:        aws.String("ec2-id-not2"),
		}},
	}
	expected := []instance{
		{
			instanceID:          "ec2-id-br1",
			containerInstanceID: "cont-inst-br1",
		},
		{
			instanceID:          "ec2-id-br2",
			containerInstanceID: "cont-inst-br2",
		},
	}

	mockECS := MockECS{
		DescribeContainerInstancesFn: func(_ *ecs.DescribeContainerInstancesInput) (*ecs.DescribeContainerInstancesOutput, error) {
			return output, nil
		},
	}
	u := updater{ecs: mockECS}

	actual, err := u.filterBottlerocketInstances([]*string{
		aws.String("ec2-id-br1"),
		aws.String("ec2-id-br2"),
		aws.String("ec2-id-not1"),
		aws.String("ec2-id-not2"),
	})
	require.NoError(t, err)
	assert.EqualValues(t, expected, actual)
}

func TestPaginatedFilterBottlerocketInstancesAllFail(t *testing.T) {
	instances := make([]*string, 0)
	descOut := make([]*ecs.ContainerInstance, 0)
	for i := 0; i < 150; i++ {
		instanceARN := "cont-inst-br" + strconv.Itoa(i)
		ec2ID := "ec2-id-br" + strconv.Itoa(i)
		instances = append(instances, aws.String(ec2ID))
		descOut = append(descOut, &ecs.ContainerInstance{
			Attributes:           []*ecs.Attribute{{Name: aws.String("bottlerocket.variant")}},
			ContainerInstanceArn: aws.String(instanceARN),
			Ec2InstanceId:        aws.String(ec2ID),
		},
		)
	}

	responses := []struct {
		inputLen           int
		ContainerInstances []*ecs.ContainerInstance
		err                error
	}{{
		100,
		descOut[:100],
		errors.New("Failed to describe container instances"),
	}, {
		50,
		descOut[100:],
		errors.New("Failed to describe container instances"),
	}}

	callCount := 0
	mockECS := MockECS{
		DescribeContainerInstancesFn: func(input *ecs.DescribeContainerInstancesInput) (*ecs.DescribeContainerInstancesOutput, error) {
			require.Less(t, callCount, len(responses))
			resp := responses[callCount]
			callCount++
			assert.Equal(t, resp.inputLen, len(input.ContainerInstances))
			return &ecs.DescribeContainerInstancesOutput{ContainerInstances: resp.ContainerInstances}, resp.err
		},
	}

	u := updater{ecs: mockECS}
	actual, err := u.filterBottlerocketInstances(instances)
	assert.Empty(t, actual)
	assert.EqualError(t, err, "failed to describe any container instances")
}

func TestPaginatedFilterBottlerocketInstancesSingleFailure(t *testing.T) {
	descOut := make([]*ecs.ContainerInstance, 0)
	instances := make([]*string, 0)
	expected := make([]instance, 0)
	for i := 0; i < 150; i++ {
		instanceARN := "cont-inst-br" + strconv.Itoa(i)
		ec2ID := "ec2-id-br" + strconv.Itoa(i)
		instances = append(instances, aws.String(ec2ID))
		descOut = append(descOut, &ecs.ContainerInstance{
			Attributes:           []*ecs.Attribute{{Name: aws.String("bottlerocket.variant")}},
			ContainerInstanceArn: aws.String(instanceARN),
			Ec2InstanceId:        aws.String(ec2ID),
		},
		)
		expected = append(expected, instance{
			instanceID:          ec2ID,
			containerInstanceID: instanceARN,
		})
	}

	responses := []struct {
		inputLen           int
		ContainerInstances []*ecs.ContainerInstance
		err                error
	}{{
		100,
		nil,
		errors.New("Failed to describe container instances"),
	}, {
		50,
		descOut[100:],
		nil,
	}}

	callCount := 0
	mockECS := MockECS{
		DescribeContainerInstancesFn: func(input *ecs.DescribeContainerInstancesInput) (*ecs.DescribeContainerInstancesOutput, error) {
			require.Less(t, callCount, len(responses))
			resp := responses[callCount]
			callCount++
			assert.Equal(t, resp.inputLen, len(input.ContainerInstances))
			return &ecs.DescribeContainerInstancesOutput{ContainerInstances: resp.ContainerInstances}, resp.err
		},
	}

	u := updater{ecs: mockECS}
	actual, err := u.filterBottlerocketInstances(instances)
	require.NoError(t, err)
	assert.EqualValues(t, expected[100:], actual)
}

func TestPaginatedFilterBottlerocketInstancesNoBR(t *testing.T) {
	descOut := make([]*ecs.ContainerInstance, 0)
	instances := make([]*string, 0)
	for i := 0; i < 150; i++ {
		instanceARN := "cont-inst-br" + strconv.Itoa(i)
		ec2ID := "ec2-id-br" + strconv.Itoa(i)
		instances = append(instances, aws.String(ec2ID))
		descOut = append(descOut, &ecs.ContainerInstance{
			Attributes:           []*ecs.Attribute{{Name: aws.String("nottlerocket.variant")}},
			ContainerInstanceArn: aws.String(instanceARN),
			Ec2InstanceId:        aws.String(ec2ID),
		},
		)
	}

	responses := []struct {
		inputLen           int
		ContainerInstances []*ecs.ContainerInstance
		err                error
	}{{
		100,
		descOut[:100],
		nil,
	}, {
		50,
		descOut[100:],
		nil,
	}}

	callCount := 0
	mockECS := MockECS{
		DescribeContainerInstancesFn: func(input *ecs.DescribeContainerInstancesInput) (*ecs.DescribeContainerInstancesOutput, error) {
			require.Less(t, callCount, len(responses))
			resp := responses[callCount]
			callCount++
			assert.Equal(t, resp.inputLen, len(input.ContainerInstances))
			return &ecs.DescribeContainerInstancesOutput{ContainerInstances: resp.ContainerInstances}, resp.err
		},
	}

	u := updater{ecs: mockECS}
	actual, err := u.filterBottlerocketInstances(instances)
	require.NoError(t, err)
	assert.Empty(t, actual)
}

func TestPaginatedFilterBottlerocketInstancesAllBRInstances(t *testing.T) {
	descOut := make([]*ecs.ContainerInstance, 0)
	instances := make([]*string, 0)
	expected := make([]instance, 0)
	for i := 0; i < 150; i++ {
		instanceARN := "cont-inst-br" + strconv.Itoa(i)
		ec2ID := "ec2-id-br" + strconv.Itoa(i)
		instances = append(instances, aws.String(ec2ID))
		descOut = append(descOut, &ecs.ContainerInstance{
			Attributes:           []*ecs.Attribute{{Name: aws.String("bottlerocket.variant")}},
			ContainerInstanceArn: aws.String(instanceARN),
			Ec2InstanceId:        aws.String(ec2ID),
		},
		)
		expected = append(expected, instance{
			instanceID:          ec2ID,
			containerInstanceID: instanceARN,
		})
	}

	responses := []struct {
		inputLen           int
		ContainerInstances []*ecs.ContainerInstance
		err                error
	}{{
		100,
		descOut[:100],
		nil,
	}, {
		50,
		descOut[100:],
		nil,
	}}

	callCount := 0
	mockECS := MockECS{
		DescribeContainerInstancesFn: func(input *ecs.DescribeContainerInstancesInput) (*ecs.DescribeContainerInstancesOutput, error) {
			require.Less(t, callCount, len(responses))
			resp := responses[callCount]
			callCount++
			assert.Equal(t, resp.inputLen, len(input.ContainerInstances))
			return &ecs.DescribeContainerInstancesOutput{ContainerInstances: resp.ContainerInstances}, resp.err
		},
	}

	u := updater{ecs: mockECS}
	actual, err := u.filterBottlerocketInstances(instances)
	require.NoError(t, err)
	assert.EqualValues(t, expected, actual)
}

func TestEligible(t *testing.T) {
	cases := []struct {
		name        string
		listOut     *ecs.ListTasksOutput
		describeOut *ecs.DescribeTasksOutput
		expectedOk  bool
	}{
		{
			name: "only service tasks",
			listOut: &ecs.ListTasksOutput{
				TaskArns: []*string{
					aws.String("task-arn-1"),
				},
			},
			describeOut: &ecs.DescribeTasksOutput{
				Tasks: []*ecs.Task{
					{
						// contains proper prefix "ecs-svc" for task started by service
						StartedBy: aws.String("ecs-svc/svc-id"),
					},
				},
			},
			expectedOk: true,
		}, {
			name: "no task",
			listOut: &ecs.ListTasksOutput{
				TaskArns: []*string{},
			},
			expectedOk: true,
		}, {
			name: "non service task",
			listOut: &ecs.ListTasksOutput{
				TaskArns: []*string{
					aws.String("task-arn-1"),
				},
			},
			describeOut: &ecs.DescribeTasksOutput{
				Tasks: []*ecs.Task{{
					// Does not contain prefix "ecs-svc"
					StartedBy: aws.String("standalone-task-id"),
				}},
			},
			expectedOk: false,
		}, {
			name: "non service task empty StartedBy",
			listOut: &ecs.ListTasksOutput{
				TaskArns: []*string{
					aws.String("task-arn-1"),
				},
			},
			describeOut: &ecs.DescribeTasksOutput{
				Tasks: []*ecs.Task{{}},
			},
			expectedOk: false,
		}, {
			name: "service and non service tasks",
			listOut: &ecs.ListTasksOutput{
				TaskArns: []*string{
					aws.String("task-arn-1"),
					aws.String("task-arn-2"),
				},
			},
			describeOut: &ecs.DescribeTasksOutput{
				Tasks: []*ecs.Task{{
					// Does not contain prefix "ecs-svc"
					StartedBy: aws.String("standalone-task-id"),
				}, {
					// contains proper prefix "ecs-svc" for task started by service
					StartedBy: aws.String("ecs-svc/svc-id"),
				}},
			},
			expectedOk: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mockECS := MockECS{
				ListTasksFn: func(input *ecs.ListTasksInput) (*ecs.ListTasksOutput, error) {
					assert.Equal(t, "test-cluster", aws.StringValue(input.Cluster))
					assert.Equal(t, "cont-inst-id", aws.StringValue(input.ContainerInstance))
					return tc.listOut, nil
				},
				DescribeTasksFn: func(input *ecs.DescribeTasksInput) (*ecs.DescribeTasksOutput, error) {
					assert.Equal(t, "test-cluster", aws.StringValue(input.Cluster))
					assert.Equal(t, tc.listOut.TaskArns, input.Tasks)
					return tc.describeOut, nil
				},
			}
			u := updater{ecs: mockECS, cluster: "test-cluster"}
			ok, err := u.eligible("cont-inst-id")
			require.NoError(t, err)
			assert.Equal(t, ok, tc.expectedOk)
		})
	}
}

func TestEligibleErr(t *testing.T) {
	t.Run("list task err", func(t *testing.T) {
		listErr := errors.New("failed to list tasks")
		mockECS := MockECS{
			ListTasksFn: func(input *ecs.ListTasksInput) (*ecs.ListTasksOutput, error) {
				assert.Equal(t, "test-cluster", aws.StringValue(input.Cluster))
				assert.Equal(t, "cont-inst-id", aws.StringValue(input.ContainerInstance))
				return nil, listErr
			},
		}
		u := updater{ecs: mockECS, cluster: "test-cluster"}
		ok, err := u.eligible("cont-inst-id")
		require.Error(t, err)
		assert.ErrorIs(t, err, listErr)
		assert.False(t, ok)
	})

	t.Run("describe task err", func(t *testing.T) {
		describeErr := errors.New("failed to describe tasks")
		mockECS := MockECS{
			ListTasksFn: func(input *ecs.ListTasksInput) (*ecs.ListTasksOutput, error) {
				assert.Equal(t, "test-cluster", aws.StringValue(input.Cluster))
				assert.Equal(t, "cont-inst-id", aws.StringValue(input.ContainerInstance))
				return &ecs.ListTasksOutput{
					TaskArns: []*string{
						aws.String("task-arn-1"),
					},
				}, nil
			},
			DescribeTasksFn: func(input *ecs.DescribeTasksInput) (*ecs.DescribeTasksOutput, error) {
				assert.Equal(t, "test-cluster", aws.StringValue(input.Cluster))
				assert.Equal(t, []*string{
					aws.String("task-arn-1"),
				}, input.Tasks)
				return nil, describeErr
			},
		}
		u := updater{ecs: mockECS, cluster: "test-cluster"}
		ok, err := u.eligible("cont-inst-id")
		require.Error(t, err)
		assert.ErrorIs(t, err, describeErr)
		assert.False(t, ok)
	})
}

func TestDrainInstance(t *testing.T) {
	stateChangeCalls := []string{}
	mockStateChange := func(input *ecs.UpdateContainerInstancesStateInput) (*ecs.UpdateContainerInstancesStateOutput, error) {
		stateChangeCalls = append(stateChangeCalls, aws.StringValue(input.Status))
		assert.Equal(t, "test-cluster", aws.StringValue(input.Cluster))
		assert.Equal(t, []*string{aws.String("cont-inst-id")}, input.ContainerInstances)
		return &ecs.UpdateContainerInstancesStateOutput{
			Failures: []*ecs.Failure{},
		}, nil
	}
	mockListTasks := func(input *ecs.ListTasksInput) (*ecs.ListTasksOutput, error) {
		assert.Equal(t, "test-cluster", aws.StringValue(input.Cluster))
		assert.Equal(t, "cont-inst-id", aws.StringValue(input.ContainerInstance))
		return &ecs.ListTasksOutput{
			TaskArns: []*string{
				aws.String("task-arn-1"),
			},
		}, nil
	}
	cleanup := func() {
		stateChangeCalls = []string{}
	}

	t.Run("no tasks success", func(t *testing.T) {
		defer cleanup()
		listTaskCount := 0
		mockECS := MockECS{
			UpdateContainerInstancesStateFn: mockStateChange,
			ListTasksFn: func(input *ecs.ListTasksInput) (*ecs.ListTasksOutput, error) {
				assert.Equal(t, "test-cluster", aws.StringValue(input.Cluster))
				assert.Equal(t, "cont-inst-id", aws.StringValue(input.ContainerInstance))
				listTaskCount++
				return &ecs.ListTasksOutput{
					TaskArns: []*string{},
				}, nil
			},
		}
		u := updater{ecs: mockECS, cluster: "test-cluster"}
		err := u.drainInstance("cont-inst-id")
		require.NoError(t, err)
		assert.Equal(t, 1, listTaskCount)
		assert.Equal(t, []string{"DRAINING"}, stateChangeCalls)
	})

	t.Run("with tasks success", func(t *testing.T) {
		defer cleanup()
		waitCount := 0
		mockECS := MockECS{
			UpdateContainerInstancesStateFn: mockStateChange,
			ListTasksFn:                     mockListTasks,
			WaitUntilTasksStoppedWithContextFn: func(ctx aws.Context, input *ecs.DescribeTasksInput, opts ...request.WaiterOption) error {
				assert.Equal(t, []*string{
					aws.String("task-arn-1"),
				}, input.Tasks)
				assert.Equal(t, "test-cluster", aws.StringValue(input.Cluster))
				waitCount++
				return nil
			},
		}
		u := updater{ecs: mockECS, cluster: "test-cluster"}
		err := u.drainInstance("cont-inst-id")
		require.NoError(t, err)
		assert.Equal(t, []string{"DRAINING"}, stateChangeCalls)
		assert.Equal(t, 1, waitCount)
	})

	t.Run("state change err", func(t *testing.T) {
		defer cleanup()
		stateOutErr := errors.New("failed to change state")
		mockECS := MockECS{
			UpdateContainerInstancesStateFn: func(input *ecs.UpdateContainerInstancesStateInput) (*ecs.UpdateContainerInstancesStateOutput, error) {
				assert.Equal(t, "test-cluster", aws.StringValue(input.Cluster))
				assert.Equal(t, []*string{aws.String("cont-inst-id")}, input.ContainerInstances)
				return nil, stateOutErr
			},
		}
		u := updater{ecs: mockECS, cluster: "test-cluster"}
		err := u.drainInstance("cont-inst-id")
		require.Error(t, err)
		assert.ErrorIs(t, err, stateOutErr)
	})

	t.Run("state change api err", func(t *testing.T) {
		defer cleanup()
		stateOutAPIFailure := &ecs.UpdateContainerInstancesStateOutput{
			Failures: []*ecs.Failure{
				{
					Reason: aws.String("failed"),
				},
			},
		}
		mockECS := MockECS{
			UpdateContainerInstancesStateFn: func(input *ecs.UpdateContainerInstancesStateInput) (*ecs.UpdateContainerInstancesStateOutput, error) {
				stateChangeCalls = append(stateChangeCalls, aws.StringValue(input.Status))
				assert.Equal(t, "test-cluster", aws.StringValue(input.Cluster))
				assert.Equal(t, []*string{aws.String("cont-inst-id")}, input.ContainerInstances)
				return stateOutAPIFailure, nil
			},
		}
		u := updater{ecs: mockECS, cluster: "test-cluster"}
		err := u.drainInstance("cont-inst-id")
		require.Error(t, err)
		assert.Contains(t, err.Error(), fmt.Sprintf("%v", stateOutAPIFailure.Failures))
		assert.Equal(t, []string{"DRAINING", "ACTIVE"}, stateChangeCalls)
	})

	t.Run("list task err", func(t *testing.T) {
		defer cleanup()
		listTaskErr := errors.New("failed to list tasks")
		mockECS := MockECS{
			UpdateContainerInstancesStateFn: mockStateChange,
			ListTasksFn: func(input *ecs.ListTasksInput) (*ecs.ListTasksOutput, error) {
				assert.Equal(t, "test-cluster", aws.StringValue(input.Cluster))
				assert.Equal(t, "cont-inst-id", aws.StringValue(input.ContainerInstance))
				return nil, listTaskErr
			},
		}
		u := updater{ecs: mockECS, cluster: "test-cluster"}
		err := u.drainInstance("cont-inst-id")
		require.Error(t, err)
		assert.ErrorIs(t, err, listTaskErr)
		assert.Equal(t, []string{"DRAINING", "ACTIVE"}, stateChangeCalls)
	})

	t.Run("wait tasks stop err", func(t *testing.T) {
		defer cleanup()
		waitTaskErr := errors.New("failed to wait for tasks to stop")
		mockECS := MockECS{
			UpdateContainerInstancesStateFn: mockStateChange,
			ListTasksFn:                     mockListTasks,
			WaitUntilTasksStoppedWithContextFn: func(ctx aws.Context, input *ecs.DescribeTasksInput, opts ...request.WaiterOption) error {
				assert.Equal(t, []*string{
					aws.String("task-arn-1"),
				}, input.Tasks)
				assert.Equal(t, "test-cluster", aws.StringValue(input.Cluster))
				return waitTaskErr
			},
		}
		u := updater{ecs: mockECS, cluster: "test-cluster"}
		err := u.drainInstance("cont-inst-id")
		require.Error(t, err)
		assert.ErrorIs(t, err, waitTaskErr)
		assert.Equal(t, []string{"DRAINING", "ACTIVE"}, stateChangeCalls)
	})
}

func TestUpdateInstance(t *testing.T) {
	checkPattern := "{\"update_state\": \"%s\", \"active_partition\": { \"image\": { \"version\": \"0.0.0\"}}}"
	cases := []struct {
		name                        string
		invocationOut               *ssm.GetCommandInvocationOutput
		expectedSSMCommandCallOrder []string
		expectedErr                 string
	}{
		{
			name: "update state available",
			invocationOut: &ssm.GetCommandInvocationOutput{
				StandardOutputContent: aws.String(fmt.Sprintf(checkPattern, updateStateAvailable)),
			},
			expectedSSMCommandCallOrder: []string{"check-document", "apply-document", "reboot-document"},
		}, {
			name: "update state ready",
			invocationOut: &ssm.GetCommandInvocationOutput{
				StandardOutputContent: aws.String(fmt.Sprintf(checkPattern, updateStateReady)),
			},
			expectedSSMCommandCallOrder: []string{"check-document", "reboot-document"},
		}, {
			name: "update state idle",
			invocationOut: &ssm.GetCommandInvocationOutput{
				StandardOutputContent: aws.String(fmt.Sprintf(checkPattern, updateStateIdle)),
			},
			expectedSSMCommandCallOrder: []string{"check-document"},
		}, {
			name: "update state staged",
			invocationOut: &ssm.GetCommandInvocationOutput{
				StandardOutputContent: aws.String(fmt.Sprintf(checkPattern, updateStateStaged)),
			},
			expectedSSMCommandCallOrder: []string{"check-document"},
			expectedErr:                 "unexpected update state \"Staged\"; skipping instance",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ssmCommandCallOrder := []string{}
			mockSSM := MockSSM{
				SendCommandFn: func(input *ssm.SendCommandInput) (*ssm.SendCommandOutput, error) {
					ssmCommandCallOrder = append(ssmCommandCallOrder, aws.StringValue(input.DocumentName))
					assert.Equal(t, []*string{aws.String("instance-id")}, input.InstanceIds)
					return &ssm.SendCommandOutput{
						Command: &ssm.Command{
							CommandId: aws.String("command-id"),
						},
					}, nil
				},
				GetCommandInvocationFn: func(input *ssm.GetCommandInvocationInput) (*ssm.GetCommandInvocationOutput, error) {
					assert.Equal(t, "command-id", aws.StringValue(input.CommandId))
					assert.Equal(t, "instance-id", aws.StringValue(input.InstanceId))
					return tc.invocationOut, nil
				},
				WaitUntilCommandExecutedWithContextFn: func(ctx aws.Context, input *ssm.GetCommandInvocationInput, opts ...request.WaiterOption) error {
					assert.Equal(t, "command-id", aws.StringValue(input.CommandId))
					assert.Equal(t, "instance-id", aws.StringValue(input.InstanceId))
					return nil
				},
			}
			mockEC2 := MockEC2{
				WaitUntilInstanceStatusOkFn: func(input *ec2.DescribeInstanceStatusInput) error {
					assert.Equal(t, []*string{aws.String("instance-id")}, input.InstanceIds)
					return nil
				},
			}
			u := updater{ssm: mockSSM, ec2: mockEC2, checkDocument: "check-document", applyDocument: "apply-document", rebootDocument: "reboot-document"}
			err := u.updateInstance(instance{
				instanceID:          "instance-id",
				containerInstanceID: "cont-inst-id",
				bottlerocketVersion: "v0.1.0",
			})
			if tc.expectedErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.expectedErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.expectedSSMCommandCallOrder, ssmCommandCallOrder)
		})
	}
}

func TestUpdateInstanceErr(t *testing.T) {
	commandOutput := &ssm.SendCommandOutput{
		Command: &ssm.Command{
			CommandId: aws.String("command-id"),
		},
	}
	mockSendCommand := func(input *ssm.SendCommandInput) (*ssm.SendCommandOutput, error) {
		assert.Equal(t, []*string{aws.String("instance-id")}, input.InstanceIds)
		return commandOutput, nil
	}
	mockGetCommandInvocation := func(input *ssm.GetCommandInvocationInput) (*ssm.GetCommandInvocationOutput, error) {
		assert.Equal(t, "command-id", aws.StringValue(input.CommandId))
		assert.Equal(t, "instance-id", aws.StringValue(input.InstanceId))
		return &ssm.GetCommandInvocationOutput{
			StandardOutputContent: aws.String("{\"update_state\": \"Available\", \"active_partition\": { \"image\": { \"version\": \"0.0.0\"}}}"),
		}, nil
	}
	mockWaitCommandExecution := func(ctx aws.Context, input *ssm.GetCommandInvocationInput, opts ...request.WaiterOption) error {
		assert.Equal(t, "command-id", aws.StringValue(input.CommandId))
		assert.Equal(t, "instance-id", aws.StringValue(input.InstanceId))
		return nil
	}

	t.Run("check err", func(t *testing.T) {
		checkErr := errors.New("failed to send check command")
		mockSSM := MockSSM{
			SendCommandFn: func(input *ssm.SendCommandInput) (*ssm.SendCommandOutput, error) {
				assert.Equal(t, "check-document", aws.StringValue(input.DocumentName))
				assert.Equal(t, []*string{aws.String("instance-id")}, input.InstanceIds)
				return nil, checkErr
			},
		}
		u := updater{ssm: mockSSM, checkDocument: "check-document"}
		err := u.updateInstance(instance{
			instanceID:          "instance-id",
			containerInstanceID: "cont-inst-id",
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, checkErr)
	})
	t.Run("apply err", func(t *testing.T) {
		applyErr := errors.New("failed to send apply command")
		mockSSM := MockSSM{
			SendCommandFn: func(input *ssm.SendCommandInput) (*ssm.SendCommandOutput, error) {
				assert.Equal(t, []*string{aws.String("instance-id")}, input.InstanceIds)
				if aws.StringValue(input.DocumentName) == "apply-document" {
					return nil, applyErr
				}
				return commandOutput, nil
			},
			GetCommandInvocationFn:                mockGetCommandInvocation,
			WaitUntilCommandExecutedWithContextFn: mockWaitCommandExecution,
		}
		u := updater{ssm: mockSSM, checkDocument: "check-document", applyDocument: "apply-document"}
		err := u.updateInstance(instance{
			instanceID:          "instance-id",
			containerInstanceID: "cont-inst-id",
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, applyErr)
	})
	t.Run("reboot err", func(t *testing.T) {
		rebootErr := errors.New("failed to send reboot command")
		mockSSM := MockSSM{
			SendCommandFn: func(input *ssm.SendCommandInput) (*ssm.SendCommandOutput, error) {
				assert.Equal(t, []*string{aws.String("instance-id")}, input.InstanceIds)
				if aws.StringValue(input.DocumentName) == "reboot-document" {
					return nil, rebootErr
				}
				return commandOutput, nil
			},
			GetCommandInvocationFn:                mockGetCommandInvocation,
			WaitUntilCommandExecutedWithContextFn: mockWaitCommandExecution,
		}
		u := updater{ssm: mockSSM, checkDocument: "check-document", applyDocument: "apply-document", rebootDocument: "reboot-document"}
		err := u.updateInstance(instance{
			instanceID:          "instance-id",
			containerInstanceID: "cont-inst-id",
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, rebootErr)
	})
	t.Run("invocation err", func(t *testing.T) {
		ssmGetInvocationErr := errors.New("failed to get command invocation")
		mockSSM := MockSSM{
			SendCommandFn: mockSendCommand,
			GetCommandInvocationFn: func(input *ssm.GetCommandInvocationInput) (*ssm.GetCommandInvocationOutput, error) {
				assert.Equal(t, "command-id", aws.StringValue(input.CommandId))
				assert.Equal(t, "instance-id", aws.StringValue(input.InstanceId))
				return nil, ssmGetInvocationErr
			},
			WaitUntilCommandExecutedWithContextFn: mockWaitCommandExecution,
		}
		u := updater{ssm: mockSSM, checkDocument: "check-document"}
		err := u.updateInstance(instance{
			instanceID:          "instance-id",
			containerInstanceID: "cont-inst-id",
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, ssmGetInvocationErr)
	})
	t.Run("wait ssm err", func(t *testing.T) {
		waitExecErr := errors.New("failed to wait ssm execution complete")
		mockSSM := MockSSM{
			SendCommandFn: mockSendCommand,
			WaitUntilCommandExecutedWithContextFn: func(ctx aws.Context, input *ssm.GetCommandInvocationInput, opts ...request.WaiterOption) error {
				assert.Equal(t, "command-id", aws.StringValue(input.CommandId))
				assert.Equal(t, "instance-id", aws.StringValue(input.InstanceId))
				return waitExecErr
			},
		}
		u := updater{ssm: mockSSM, checkDocument: "check-document"}
		err := u.updateInstance(instance{
			instanceID:          "instance-id",
			containerInstanceID: "cont-inst-id",
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, waitExecErr)
	})
	t.Run("wait instance ok err", func(t *testing.T) {
		waitErr := errors.New("failed to wait instance ok")
		mockSSM := MockSSM{
			SendCommandFn:                         mockSendCommand,
			GetCommandInvocationFn:                mockGetCommandInvocation,
			WaitUntilCommandExecutedWithContextFn: mockWaitCommandExecution,
		}

		mockEC2 := MockEC2{
			WaitUntilInstanceStatusOkFn: func(input *ec2.DescribeInstanceStatusInput) error {
				assert.Equal(t, []*string{aws.String("instance-id")}, input.InstanceIds)
				return waitErr
			},
		}
		u := updater{ssm: mockSSM, ec2: mockEC2, checkDocument: "check-document", applyDocument: "apply-document", rebootDocument: "reboot-document"}
		err := u.updateInstance(instance{
			instanceID:          "instance-id",
			containerInstanceID: "cont-inst-id",
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, waitErr)
	})
}

func TestVerifyUpdate(t *testing.T) {
	checkPattern := "{\"update_state\": \"%s\", \"active_partition\": { \"image\": { \"version\": \"%s\"}}}"
	cases := []struct {
		name          string
		invocationOut *ssm.GetCommandInvocationOutput
		expectedOk    bool
	}{
		{
			name: "verify success",
			invocationOut: &ssm.GetCommandInvocationOutput{
				StandardOutputContent: aws.String(fmt.Sprintf(checkPattern, updateStateIdle, "0.0.1")),
			},
			expectedOk: true,
		},
		{
			name: "version is same",
			invocationOut: &ssm.GetCommandInvocationOutput{
				StandardOutputContent: aws.String(fmt.Sprintf(checkPattern, updateStateIdle, "0.0.0")),
			},
			expectedOk: false,
		},
		{
			name: "another version is available",
			invocationOut: &ssm.GetCommandInvocationOutput{
				StandardOutputContent: aws.String(fmt.Sprintf(checkPattern, updateStateAvailable, "0.0.1")),
			},
			expectedOk: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mockSSM := MockSSM{
				SendCommandFn: func(input *ssm.SendCommandInput) (*ssm.SendCommandOutput, error) {
					assert.Equal(t, "check-document", aws.StringValue(input.DocumentName))
					return &ssm.SendCommandOutput{
						Command: &ssm.Command{
							CommandId: aws.String("command-id"),
						},
					}, nil
				},
				GetCommandInvocationFn: func(input *ssm.GetCommandInvocationInput) (*ssm.GetCommandInvocationOutput, error) {
					assert.Equal(t, "command-id", aws.StringValue(input.CommandId))
					assert.Equal(t, "instance-id", aws.StringValue(input.InstanceId))
					return tc.invocationOut, nil
				},
				WaitUntilCommandExecutedWithContextFn: func(ctx aws.Context, input *ssm.GetCommandInvocationInput, opts ...request.WaiterOption) error {
					assert.Equal(t, "command-id", aws.StringValue(input.CommandId))
					assert.Equal(t, "instance-id", aws.StringValue(input.InstanceId))
					return nil
				},
			}
			u := updater{ssm: mockSSM, checkDocument: "check-document"}
			ok, err := u.verifyUpdate(instance{
				instanceID:          "instance-id",
				containerInstanceID: "cont-inst-id",
				bottlerocketVersion: "0.0.0",
			})
			require.NoError(t, err)
			assert.Equal(t, tc.expectedOk, ok)
		})
	}
}

func TestVerifyUpdateErr(t *testing.T) {
	mockSSMCommandOut := func(input *ssm.SendCommandInput) (*ssm.SendCommandOutput, error) {
		assert.Equal(t, "check-document", aws.StringValue(input.DocumentName))
		assert.Equal(t, 1, len(input.InstanceIds))
		assert.Equal(t, "instance-id", aws.StringValue(input.InstanceIds[0]))
		return &ssm.SendCommandOutput{
			Command: &ssm.Command{
				CommandId: aws.String("command-id"),
			},
		}, nil
	}
	mockWaitCommandExecution := func(ctx aws.Context, input *ssm.GetCommandInvocationInput, opts ...request.WaiterOption) error {
		assert.Equal(t, "command-id", aws.StringValue(input.CommandId))
		assert.Equal(t, "instance-id", aws.StringValue(input.InstanceId))
		return nil
	}
	mockGetCommandInvocation := func(input *ssm.GetCommandInvocationInput) (*ssm.GetCommandInvocationOutput, error) {
		assert.Equal(t, "command-id", aws.StringValue(input.CommandId))
		assert.Equal(t, "instance-id", aws.StringValue(input.InstanceId))
		return &ssm.GetCommandInvocationOutput{}, nil
	}
	t.Run("check err", func(t *testing.T) {
		ssmCheckErr := errors.New("failed to send check command")
		mockSSM := MockSSM{
			SendCommandFn: func(input *ssm.SendCommandInput) (*ssm.SendCommandOutput, error) {
				assert.Equal(t, "check-document", aws.StringValue(input.DocumentName))
				assert.Equal(t, 1, len(input.InstanceIds))
				assert.Equal(t, "instance-id", aws.StringValue(input.InstanceIds[0]))
				return nil, ssmCheckErr
			},
		}
		u := updater{ssm: mockSSM, checkDocument: "check-document"}
		ok, err := u.verifyUpdate(instance{
			instanceID:          "instance-id",
			containerInstanceID: "cont-inst-id",
			bottlerocketVersion: "0.0.0",
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, ssmCheckErr)
		assert.False(t, ok)
	})
	t.Run("wait ssm err", func(t *testing.T) {
		waitExecErr := errors.New("failed to wait ssm execution complete")
		mockSSM := MockSSM{
			SendCommandFn: mockSSMCommandOut,
			WaitUntilCommandExecutedWithContextFn: func(ctx aws.Context, input *ssm.GetCommandInvocationInput, opts ...request.WaiterOption) error {
				assert.Equal(t, "command-id", aws.StringValue(input.CommandId))
				assert.Equal(t, "instance-id", aws.StringValue(input.InstanceId))
				return waitExecErr
			},
		}
		u := updater{ssm: mockSSM, checkDocument: "check-document"}
		ok, err := u.verifyUpdate(instance{
			instanceID:          "instance-id",
			containerInstanceID: "cont-inst-id",
			bottlerocketVersion: "0.0.0",
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, waitExecErr)
		assert.False(t, ok)
	})
	t.Run("invocation err", func(t *testing.T) {
		ssmGetInvocationErr := errors.New("failed to get command invocation")
		mockSSM := MockSSM{
			SendCommandFn:                         mockSSMCommandOut,
			WaitUntilCommandExecutedWithContextFn: mockWaitCommandExecution,
			GetCommandInvocationFn: func(input *ssm.GetCommandInvocationInput) (*ssm.GetCommandInvocationOutput, error) {
				assert.Equal(t, "command-id", aws.StringValue(input.CommandId))
				assert.Equal(t, "instance-id", aws.StringValue(input.InstanceId))
				return nil, ssmGetInvocationErr
			},
		}
		u := updater{ssm: mockSSM, checkDocument: "check-document"}
		ok, err := u.verifyUpdate(instance{
			instanceID:          "instance-id",
			containerInstanceID: "cont-inst-id",
			bottlerocketVersion: "0.0.0",
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, ssmGetInvocationErr)
		assert.False(t, ok)
	})

	t.Run("parse output err", func(t *testing.T) {
		mockSSM := MockSSM{
			SendCommandFn:                         mockSSMCommandOut,
			WaitUntilCommandExecutedWithContextFn: mockWaitCommandExecution,
			GetCommandInvocationFn:                mockGetCommandInvocation,
		}
		u := updater{ssm: mockSSM, checkDocument: "check-document"}
		ok, err := u.verifyUpdate(instance{
			instanceID:          "instance-id",
			containerInstanceID: "cont-inst-id",
			bottlerocketVersion: "0.0.0",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to parse command output , manual verification required")
		assert.False(t, ok)
	})
}
