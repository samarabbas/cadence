// Copyright (c) 2017 Uber Technologies, Inc.
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

package history

import (
	"fmt"
	"time"

	"github.com/uber-common/bark"
	h "github.com/uber/cadence/.gen/go/history"
	workflow "github.com/uber/cadence/.gen/go/shared"
	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/backoff"
	"github.com/uber/cadence/common/logging"
	"github.com/uber/cadence/common/persistence"
)

const (
	secondsInDay = int32(24 * time.Hour / time.Second)
)

type (
	workflowExecutionContext struct {
		domainID          string
		workflowExecution workflow.WorkflowExecution
		shard             ShardContext
		executionManager  persistence.ExecutionManager
		logger            bark.Logger

		locker          common.Mutex
		msBuilder       *mutableStateBuilder
		updateCondition int64
		deleteTimerTask persistence.Task
	}
)

var (
	persistenceOperationRetryPolicy = common.CreatePersistanceRetryPolicy()
)

func newWorkflowExecutionContext(domainID string, execution workflow.WorkflowExecution, shard ShardContext,
	executionManager persistence.ExecutionManager, logger bark.Logger) *workflowExecutionContext {
	lg := logger.WithFields(bark.Fields{
		logging.TagWorkflowExecutionID: *execution.WorkflowId,
		logging.TagWorkflowRunID:       *execution.RunId,
	})

	return &workflowExecutionContext{
		domainID:          domainID,
		workflowExecution: execution,
		shard:             shard,
		executionManager:  executionManager,
		logger:            lg,
		locker:            common.NewMutex(),
	}
}

func (c *workflowExecutionContext) loadWorkflowExecution() (*mutableStateBuilder, error) {
	if c.msBuilder != nil {
		if err := c.updateVersion(); err != nil {
			return nil, err
		}
		return c.msBuilder, nil
	}

	response, err := c.getWorkflowExecutionWithRetry(&persistence.GetWorkflowExecutionRequest{
		DomainID:  c.domainID,
		Execution: c.workflowExecution,
	})
	if err != nil {
		if common.IsPersistenceTransientError(err) {
			logging.LogPersistantStoreErrorEvent(c.logger, logging.TagValueStoreOperationGetWorkflowExecution, err, "")
		}
		return nil, err
	}

	msBuilder := newMutableStateBuilder(c.shard.GetConfig(), c.logger)
	if response != nil && response.State != nil {
		state := response.State
		msBuilder.Load(state)
		info := state.ExecutionInfo
		c.updateCondition = info.NextEventID
	}

	c.msBuilder = msBuilder
	if err := c.updateVersion(); err != nil {
		return nil, err
	}
	return msBuilder, nil
}

func (c *workflowExecutionContext) resetWorkflowExecution(resetBuilder *mutableStateBuilder) (*mutableStateBuilder,
	error) {
	snapshotRequest := resetBuilder.ResetSnapshot()
	snapshotRequest.Condition = c.updateCondition

	err := c.shard.ResetMutableState(snapshotRequest)
	if err != nil {
		return nil, err
	}

	c.clear()
	return c.loadWorkflowExecution()
}

func (c *workflowExecutionContext) updateWorkflowExecutionWithContext(context []byte, transferTasks []persistence.Task,
	timerTasks []persistence.Task, transactionID int64) error {
	c.msBuilder.executionInfo.ExecutionContext = context

	return c.updateWorkflowExecution(transferTasks, timerTasks, transactionID)
}

func (c *workflowExecutionContext) updateWorkflowExecutionWithDeleteTask(transferTasks []persistence.Task,
	timerTasks []persistence.Task, deleteTimerTask persistence.Task, transactionID int64) error {
	c.deleteTimerTask = deleteTimerTask

	return c.updateWorkflowExecution(transferTasks, timerTasks, transactionID)
}

func (c *workflowExecutionContext) replicateWorkflowExecution(request *h.ReplicateEventsRequest,
	transferTasks []persistence.Task, timerTasks []persistence.Task, lastEventID, transactionID int64) error {
	nextEventID := lastEventID + 1
	c.msBuilder.executionInfo.NextEventID = nextEventID

	builder := newHistoryBuilderFromEvents(request.History.Events, c.logger)
	return c.updateHelper(builder, transferTasks, timerTasks, false, request.GetSourceCluster(), request.GetVersion(),
		transactionID)
}

func (c *workflowExecutionContext) updateVersion() error {
	if c.shard.GetService().GetClusterMetadata().IsGlobalDomainEnabled() && c.msBuilder.replicationState != nil {

		if !c.msBuilder.isWorkflowExecutionRunning() {
			// we should not update the version on mutable state when the workflow is finished
			return nil
		}
		// Support for global domains is enabled and we are performing an update for global domain
		domainEntry, err := c.shard.GetDomainCache().GetDomainByID(c.msBuilder.executionInfo.DomainID)
		if err != nil {
			return err
		}
		c.msBuilder.updateReplicationStateVersion(domainEntry.GetFailoverVersion())
	}
	return nil
}

func (c *workflowExecutionContext) updateWorkflowExecution(transferTasks []persistence.Task,
	timerTasks []persistence.Task, transactionID int64) error {

	// Only generate replication task if this is a global domain
	lastWriteVersion := c.msBuilder.GetCurrentVersion()
	createReplicationTask := c.msBuilder.replicationState != nil
	return c.updateHelper(nil, transferTasks, timerTasks, createReplicationTask, "", lastWriteVersion, transactionID)
}

func (c *workflowExecutionContext) updateHelper(builder *historyBuilder, transferTasks []persistence.Task,
	timerTasks []persistence.Task, createReplicationTask bool, sourceCluster string, lastWriteVersion int64,
	transactionID int64) (errRet error) {

	defer func() {
		if errRet != nil {
			// Clear all cached state in case of error
			c.clear()
		}
	}()

	// Take a snapshot of all updates we have accumulated for this execution
	updates, err := c.msBuilder.CloseUpdateSession()
	if err != nil {
		return err
	}

	// Replication state should only be updated after the UpdateSession is closed.  IDs for certain events are only
	// generated on CloseSession as they could be buffered events.  The value for NextEventID will be wrong on
	// mutable state if read before flushing the buffered events.
	crossDCEnabled := c.msBuilder.replicationState != nil
	if crossDCEnabled {
		lastEventID := c.msBuilder.GetNextEventID() - 1
		c.msBuilder.updateReplicationStateLastEventID(sourceCluster, lastWriteVersion, lastEventID)
	}

	// Replicator passes in a custom builder as it already has the events
	if builder == nil {
		// If no builder is passed in then use the one as part of the updates
		builder = updates.newEventsBuilder
	}

	// Some operations only update the mutable state. For example RecordActivityTaskHeartbeat.
	if builder.history != nil && len(builder.history) > 0 {
		firstEvent := builder.GetFirstEvent()
		// Transient decision events need to be written as a separate batch
		if builder.HasTransientEvents() {
			err = c.appendHistoryEvents(builder, builder.transientHistory, transactionID)
			if err != nil {
				return err
			}
		}

		err = c.appendHistoryEvents(builder, builder.history, transactionID)
		if err != nil {
			return err
		}

		c.msBuilder.executionInfo.LastFirstEventID = *firstEvent.EventId
	}

	continueAsNew := updates.continueAsNew
	finishExecution := false
	var finishExecutionTTL int32
	if c.msBuilder.executionInfo.State == persistence.WorkflowStateCompleted {
		// Workflow execution completed as part of this transaction.
		// Also transactionally delete workflow execution representing
		// current run for the execution using cassandra TTL
		finishExecution = true
		domainEntry, err := c.shard.GetDomainCache().GetDomainByID(c.msBuilder.executionInfo.DomainID)
		if err != nil {
			return err
		}
		// NOTE: domain retention is in days, so we need to do a conversion
		finishExecutionTTL = domainEntry.GetConfig().Retention * secondsInDay
	}

	var replicationTasks []persistence.Task
	if createReplicationTask {
		// Let's create a replication task as part of this update
		replicationTasks = append(replicationTasks, c.msBuilder.createReplicationTask())
	}

	setTaskVersion(c.msBuilder.GetCurrentVersion(), transferTasks, timerTasks)

	if err1 := c.updateWorkflowExecutionWithRetry(&persistence.UpdateWorkflowExecutionRequest{
		ExecutionInfo:                 c.msBuilder.executionInfo,
		ReplicationState:              c.msBuilder.replicationState,
		TransferTasks:                 transferTasks,
		ReplicationTasks:              replicationTasks,
		TimerTasks:                    timerTasks,
		Condition:                     c.updateCondition,
		DeleteTimerTask:               c.deleteTimerTask,
		UpsertActivityInfos:           updates.updateActivityInfos,
		DeleteActivityInfos:           updates.deleteActivityInfos,
		UpserTimerInfos:               updates.updateTimerInfos,
		DeleteTimerInfos:              updates.deleteTimerInfos,
		UpsertChildExecutionInfos:     updates.updateChildExecutionInfos,
		DeleteChildExecutionInfo:      updates.deleteChildExecutionInfo,
		UpsertRequestCancelInfos:      updates.updateCancelExecutionInfos,
		DeleteRequestCancelInfo:       updates.deleteCancelExecutionInfo,
		UpsertSignalInfos:             updates.updateSignalInfos,
		DeleteSignalInfo:              updates.deleteSignalInfo,
		UpsertSignalRequestedIDs:      updates.updateSignalRequestedIDs,
		DeleteSignalRequestedID:       updates.deleteSignalRequestedID,
		NewBufferedEvents:             updates.newBufferedEvents,
		ClearBufferedEvents:           updates.clearBufferedEvents,
		NewBufferedReplicationTask:    updates.newBufferedReplicationEventsInfo,
		DeleteBufferedReplicationTask: updates.deleteBufferedReplicationEvent,
		ContinueAsNew:                 continueAsNew,
		FinishExecution:               finishExecution,
		FinishedExecutionTTL:          finishExecutionTTL,
	}); err1 != nil {
		switch err1.(type) {
		case *persistence.ConditionFailedError:
			return ErrConflict
		}

		logging.LogPersistantStoreErrorEvent(c.logger, logging.TagValueStoreOperationUpdateWorkflowExecution, err1,
			fmt.Sprintf("{updateCondition: %v}", c.updateCondition))
		return err1
	}

	// Update went through so update the condition for new updates
	c.updateCondition = c.msBuilder.GetNextEventID()
	c.msBuilder.executionInfo.LastUpdatedTimestamp = time.Now()

	// for any change in the workflow, send a event
	c.shard.NotifyNewHistoryEvent(newHistoryEventNotification(
		c.domainID,
		&c.workflowExecution,
		c.msBuilder.GetLastFirstEventID(),
		c.msBuilder.GetNextEventID(),
		c.msBuilder.isWorkflowExecutionRunning(),
	))

	return nil
}

func (c *workflowExecutionContext) appendHistoryEvents(builder *historyBuilder, history []*workflow.HistoryEvent,
	transactionID int64) error {

	firstEvent := history[0]
	serializedHistory, err := builder.SerializeEvents(history)
	if err != nil {
		logging.LogHistorySerializationErrorEvent(c.logger, err, "Unable to serialize execution history for update.")
		return err
	}

	if err0 := c.shard.AppendHistoryEvents(&persistence.AppendHistoryEventsRequest{
		DomainID:      c.domainID,
		Execution:     c.workflowExecution,
		TransactionID: transactionID,
		FirstEventID:  *firstEvent.EventId,
		Events:        serializedHistory,
	}); err0 != nil {
		switch err0.(type) {
		case *persistence.ConditionFailedError:
			return ErrConflict
		}

		logging.LogPersistantStoreErrorEvent(c.logger, logging.TagValueStoreOperationUpdateWorkflowExecution, err0,
			fmt.Sprintf("{updateCondition: %v}", c.updateCondition))
		return err0
	}

	return nil
}

func (c *workflowExecutionContext) replicateContinueAsNewWorkflowExecution(newStateBuilder *mutableStateBuilder,
	transferTasks []persistence.Task, timerTasks []persistence.Task, transactionID int64) error {
	return c.continueAsNewWorkflowExecutionHelper(nil, newStateBuilder, transferTasks, timerTasks, transactionID)
}

func (c *workflowExecutionContext) continueAsNewWorkflowExecution(context []byte, newStateBuilder *mutableStateBuilder,
	transferTasks []persistence.Task, timerTasks []persistence.Task, transactionID int64) error {

	err1 := c.continueAsNewWorkflowExecutionHelper(context, newStateBuilder, transferTasks, timerTasks, transactionID)
	if err1 != nil {
		return err1
	}

	err2 := c.updateWorkflowExecutionWithContext(context, transferTasks, timerTasks, transactionID)

	if err2 != nil {
		// TODO: Delete new execution if update fails due to conflict or shard being lost
	}

	return err2
}

func (c *workflowExecutionContext) continueAsNewWorkflowExecutionHelper(context []byte, newStateBuilder *mutableStateBuilder,
	transferTasks []persistence.Task, timerTasks []persistence.Task, transactionID int64) error {

	domainID := newStateBuilder.executionInfo.DomainID
	newExecution := workflow.WorkflowExecution{
		WorkflowId: common.StringPtr(newStateBuilder.executionInfo.WorkflowID),
		RunId:      common.StringPtr(newStateBuilder.executionInfo.RunID),
	}
	firstEvent := newStateBuilder.hBuilder.history[0]

	// Serialize the history
	serializedHistory, serializedError := newStateBuilder.hBuilder.Serialize()
	if serializedError != nil {
		logging.LogHistorySerializationErrorEvent(c.logger, serializedError, fmt.Sprintf(
			"HistoryEventBatch serialization error on start workflow.  WorkflowID: %v, RunID: %v", *newExecution.WorkflowId,
			*newExecution.RunId))
		return serializedError
	}

	return c.shard.AppendHistoryEvents(&persistence.AppendHistoryEventsRequest{
		DomainID:      domainID,
		Execution:     newExecution,
		TransactionID: transactionID,
		FirstEventID:  *firstEvent.EventId,
		Events:        serializedHistory,
	})
}

func (c *workflowExecutionContext) getWorkflowExecutionWithRetry(
	request *persistence.GetWorkflowExecutionRequest) (*persistence.GetWorkflowExecutionResponse, error) {
	var response *persistence.GetWorkflowExecutionResponse
	op := func() error {
		var err error
		response, err = c.executionManager.GetWorkflowExecution(request)

		return err
	}

	err := backoff.Retry(op, persistenceOperationRetryPolicy, common.IsPersistenceTransientError)
	if err != nil {
		return nil, err
	}

	return response, nil
}

func (c *workflowExecutionContext) updateWorkflowExecutionWithRetry(
	request *persistence.UpdateWorkflowExecutionRequest) error {
	op := func() error {
		return c.shard.UpdateWorkflowExecution(request)
	}

	return backoff.Retry(op, persistenceOperationRetryPolicy, common.IsPersistenceTransientError)
}

func (c *workflowExecutionContext) clear() {
	c.msBuilder = nil
}
