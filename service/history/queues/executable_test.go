// The MIT License
//
// Copyright (c) 2020 Temporal Technologies Inc.  All rights reserved.
//
// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package queues

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"

	"go.temporal.io/server/common/clock"
	"go.temporal.io/server/common/cluster"
	"go.temporal.io/server/common/definition"
	"go.temporal.io/server/common/headers"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/metrics"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/common/persistence/serialization"
	ctasks "go.temporal.io/server/common/tasks"
	"go.temporal.io/server/service/history/consts"
	"go.temporal.io/server/service/history/tasks"
	"go.temporal.io/server/service/history/tests"
)

type (
	executableSuite struct {
		suite.Suite
		*require.Assertions

		controller            *gomock.Controller
		mockExecutor          *MockExecutor
		mockScheduler         *MockScheduler
		mockRescheduler       *MockRescheduler
		mockNamespaceRegistry *namespace.MockRegistry
		mockClusterMetadata   *cluster.MockMetadata

		timeSource *clock.EventTimeSource
	}
)

func TestExecutableSuite(t *testing.T) {
	s := new(executableSuite)
	suite.Run(t, s)
}

func (s *executableSuite) SetupTest() {
	s.Assertions = require.New(s.T())

	s.controller = gomock.NewController(s.T())
	s.mockExecutor = NewMockExecutor(s.controller)
	s.mockScheduler = NewMockScheduler(s.controller)
	s.mockRescheduler = NewMockRescheduler(s.controller)
	s.mockNamespaceRegistry = namespace.NewMockRegistry(s.controller)
	s.mockClusterMetadata = cluster.NewMockMetadata(s.controller)

	s.mockNamespaceRegistry.EXPECT().GetNamespaceName(gomock.Any()).Return(tests.Namespace, nil).AnyTimes()
	s.mockNamespaceRegistry.EXPECT().GetNamespaceByID(gomock.Any()).Return(tests.GlobalNamespaceEntry, nil).AnyTimes()
	s.mockClusterMetadata.EXPECT().GetCurrentClusterName().Return(cluster.TestCurrentClusterName).AnyTimes()

	s.timeSource = clock.NewEventTimeSource()
}

func (s *executableSuite) TearDownSuite() {
	s.controller.Finish()
}

func (s *executableSuite) TestExecute_TaskExecuted() {
	executable := s.newTestExecutable()

	s.mockExecutor.EXPECT().Execute(gomock.Any(), executable).Return(nil, true, errors.New("some random error"))
	s.Error(executable.Execute())

	s.mockExecutor.EXPECT().Execute(gomock.Any(), executable).Return(nil, true, nil)
	s.NoError(executable.Execute())
}

func (s *executableSuite) TestExecute_InMemoryNoUserLatency() {
	executable := s.newTestExecutable()

	scheduleLatency := 100 * time.Millisecond
	userLatency := 500 * time.Millisecond
	attemptLatency := time.Second

	now := time.Now()
	s.timeSource.Update(now)
	executable.SetScheduledTime(now)

	now = now.Add(scheduleLatency)
	s.timeSource.Update(now)

	s.mockExecutor.EXPECT().Execute(gomock.Any(), executable).Do(func(ctx context.Context, taskInfo interface{}) {
		metrics.ContextCounterAdd(
			ctx,
			metrics.HistoryWorkflowExecutionCacheLatency.GetMetricName(),
			int64(userLatency),
		)

		now = now.Add(attemptLatency)
		s.timeSource.Update(now)
	}).Return(nil, true, consts.ErrResourceExhaustedBusyWorkflow)
	err := executable.HandleErr(executable.Execute())
	s.Equal(consts.ErrResourceExhaustedBusyWorkflow, err)

	s.mockScheduler.EXPECT().TrySubmit(executable).Return(false)
	s.mockRescheduler.EXPECT().Add(executable, gomock.Any())
	executable.Nack(err)

	// backoff duration
	now = now.Add(time.Second)
	s.timeSource.Update(now)

	executable.SetScheduledTime(now)
	now = now.Add(scheduleLatency)
	s.timeSource.Update(now)

	s.mockExecutor.EXPECT().Execute(gomock.Any(), executable).Do(func(ctx context.Context, taskInfo interface{}) {
		metrics.ContextCounterAdd(
			ctx,
			metrics.HistoryWorkflowExecutionCacheLatency.GetMetricName(),
			int64(userLatency),
		)

		now = now.Add(attemptLatency)
		s.timeSource.Update(now)
	}).Return(nil, true, nil)
	err = executable.HandleErr(executable.Execute())
	s.NoError(err)

	s.Equal(scheduleLatency+attemptLatency-userLatency, executable.(*executableImpl).inMemoryNoUserLatency)
}

func (s *executableSuite) TestExecute_CapturePanic() {
	executable := s.newTestExecutable()

	s.mockExecutor.EXPECT().Execute(gomock.Any(), executable).DoAndReturn(
		func(_ context.Context, _ Executable) ([]metrics.Tag, bool, error) {
			panic("test panic during execution")
		},
	)
	s.Error(executable.Execute())
}

func (s *executableSuite) TestExecute_CallerInfo() {
	executable := s.newTestExecutable()

	s.mockExecutor.EXPECT().Execute(gomock.Any(), executable).DoAndReturn(
		func(ctx context.Context, _ Executable) ([]metrics.Tag, bool, error) {
			s.Equal(headers.CallerTypeBackground, headers.GetCallerInfo(ctx).CallerType)
			return nil, true, nil
		},
	)
	s.NoError(executable.Execute())

	// force set to low priority
	executable.(*executableImpl).priority = ctasks.PriorityLow
	s.mockExecutor.EXPECT().Execute(gomock.Any(), executable).DoAndReturn(
		func(ctx context.Context, _ Executable) ([]metrics.Tag, bool, error) {
			s.Equal(headers.CallerTypePreemptable, headers.GetCallerInfo(ctx).CallerType)
			return nil, true, nil
		},
	)
	s.NoError(executable.Execute())
}

func (s *executableSuite) TestExecuteHandleErr_ResetAttempt() {
	executable := s.newTestExecutable()
	s.mockExecutor.EXPECT().Execute(gomock.Any(), executable).Return(nil, true, errors.New("some random error"))
	err := executable.Execute()
	s.Error(err)
	s.Error(executable.HandleErr(err))
	s.Equal(2, executable.Attempt())

	// isActive changed to false, should reset attempt
	s.mockExecutor.EXPECT().Execute(gomock.Any(), executable).Return(nil, false, nil)
	s.NoError(executable.Execute())
	s.Equal(1, executable.Attempt())
}

func (s *executableSuite) TestExecuteHandleErr_Corrupted() {
	executable := s.newTestExecutable()

	s.mockExecutor.EXPECT().Execute(gomock.Any(), executable).DoAndReturn(
		func(_ context.Context, _ Executable) ([]metrics.Tag, bool, error) {
			panic(serialization.NewUnknownEncodingTypeError("unknownEncoding", enumspb.ENCODING_TYPE_PROTO3))
		},
	)
	err := executable.Execute()
	s.Error(err)
	s.NoError(executable.HandleErr(err))
}

func (s *executableSuite) TestHandleErr_EntityNotExists() {
	executable := s.newTestExecutable()

	s.NoError(executable.HandleErr(serviceerror.NewNotFound("")))
}

func (s *executableSuite) TestHandleErr_ErrTaskRetry() {
	executable := s.newTestExecutable()

	s.Equal(consts.ErrTaskRetry, executable.HandleErr(consts.ErrTaskRetry))
}

func (s *executableSuite) TestHandleErr_ErrDeleteOpenExecution() {
	executable := s.newTestExecutable()

	s.Equal(consts.ErrDependencyTaskNotCompleted, executable.HandleErr(consts.ErrDependencyTaskNotCompleted))
}

func (s *executableSuite) TestHandleErr_ErrTaskDiscarded() {
	executable := s.newTestExecutable()

	s.NoError(executable.HandleErr(consts.ErrTaskDiscarded))
}

func (s *executableSuite) TestHandleErr_ErrTaskVersionMismatch() {
	executable := s.newTestExecutable()

	s.NoError(executable.HandleErr(consts.ErrTaskVersionMismatch))
}

func (s *executableSuite) TestHandleErr_NamespaceNotActiveError() {
	err := serviceerror.NewNamespaceNotActive("", "", "")

	s.Equal(err, s.newTestExecutable().HandleErr(err))
}

func (s *executableSuite) TestHandleErr_RandomErr() {
	executable := s.newTestExecutable()

	s.Error(executable.HandleErr(errors.New("random error")))
}

func (s *executableSuite) TestTaskAck() {
	executable := s.newTestExecutable()

	s.Equal(ctasks.TaskStatePending, executable.State())

	executable.Ack()
	s.Equal(ctasks.TaskStateAcked, executable.State())
}

func (s *executableSuite) TestTaskNack_Resubmit() {
	executable := s.newTestExecutable()

	s.mockScheduler.EXPECT().TrySubmit(executable).Return(true)

	executable.Nack(errors.New("some random error"))
	s.Equal(ctasks.TaskStatePending, executable.State())
}

func (s *executableSuite) TestTaskNack_Reschedule() {
	executable := s.newTestExecutable()

	s.mockRescheduler.EXPECT().Add(executable, gomock.AssignableToTypeOf(time.Now())).MinTimes(1)

	s.Run("ErrTaskRetry", func() {
		executable.Nack(consts.ErrTaskRetry) // this error won't trigger re-submit
		s.Equal(ctasks.TaskStatePending, executable.State())
	})
	s.Run("ErrDeleteOpenExecErr", func() {
		executable.Nack(consts.ErrDependencyTaskNotCompleted) // this error won't trigger re-submit
		s.Equal(ctasks.TaskStatePending, executable.State())
	})
}

func (s *executableSuite) TestTaskAbort() {
	executable := s.newTestExecutable()

	executable.Abort()

	s.NoError(executable.Execute()) // should be no-op and won't invoke executor

	executable.Ack() // should be no-op
	s.Equal(ctasks.TaskStateAborted, executable.State())

	executable.Nack(errors.New("some random error")) // should be no-op and won't invoke scheduler or rescheduler

	executable.Reschedule() // should be no-op and won't invoke rescheduler

	// all error should be treated as non-retryable to break retry loop
	s.False(executable.IsRetryableError(errors.New("some random error")))
}

func (s *executableSuite) TestTaskCancellation() {
	executable := s.newTestExecutable()

	executable.Cancel()

	s.NoError(executable.Execute()) // should be no-op and won't invoke executor

	executable.Ack() // should be no-op
	s.Equal(ctasks.TaskStateCancelled, executable.State())

	executable.Nack(errors.New("some random error")) // should be no-op and won't invoke scheduler or rescheduler

	executable.Reschedule() // should be no-op and won't invoke rescheduler

	// all error should be treated as non-retryable to break retry loop
	s.False(executable.IsRetryableError(errors.New("some random error")))
}

func (s *executableSuite) newTestExecutable() Executable {
	return NewExecutable(
		DefaultReaderId,
		tasks.NewFakeTask(
			definition.NewWorkflowKey(
				tests.NamespaceID.String(),
				tests.WorkflowID,
				tests.RunID,
			),
			tasks.CategoryTransfer,
			s.timeSource.Now(),
		),
		s.mockExecutor,
		s.mockScheduler,
		s.mockRescheduler,
		NewNoopPriorityAssigner(),
		s.timeSource,
		s.mockNamespaceRegistry,
		s.mockClusterMetadata,
		log.NewTestLogger(),
		metrics.NoopMetricsHandler,
	)
}
