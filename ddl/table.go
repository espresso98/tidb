// Copyright 2015 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ddl

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/tidb/ddl/label"
	"github.com/pingcap/tidb/ddl/placement"
	"github.com/pingcap/tidb/ddl/util"
	"github.com/pingcap/tidb/domain/infosync"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/meta"
	"github.com/pingcap/tidb/meta/autoid"
	"github.com/pingcap/tidb/parser/ast"
	"github.com/pingcap/tidb/parser/charset"
	"github.com/pingcap/tidb/parser/model"
	field_types "github.com/pingcap/tidb/parser/types"
	"github.com/pingcap/tidb/table"
	"github.com/pingcap/tidb/table/tables"
	"github.com/pingcap/tidb/tablecodec"
	tidb_util "github.com/pingcap/tidb/util"
	"github.com/pingcap/tidb/util/gcutil"
)

const tiflashCheckTiDBHTTPAPIHalfInterval = 2500 * time.Millisecond

func onCreateTable(d *ddlCtx, t *meta.Meta, job *model.Job) (ver int64, _ error) {
	failpoint.Inject("mockExceedErrorLimit", func(val failpoint.Value) {
		if val.(bool) {
			failpoint.Return(ver, errors.New("mock do job error"))
		}
	})

	schemaID := job.SchemaID
	tbInfo := &model.TableInfo{}
	if err := job.DecodeArgs(tbInfo); err != nil {
		// Invalid arguments, cancel this job.
		job.State = model.JobStateCancelled
		return ver, errors.Trace(err)
	}

	tbInfo.State = model.StateNone
	err := checkTableNotExists(d, t, schemaID, tbInfo.Name.L)
	if err != nil {
		if infoschema.ErrDatabaseNotExists.Equal(err) || infoschema.ErrTableExists.Equal(err) {
			job.State = model.JobStateCancelled
		}
		return ver, errors.Trace(err)
	}
	// placement rules meta inheritance.
	dbInfo, err := checkSchemaExistAndCancelNotExistJob(t, job)
	if err != nil {
		return ver, errors.Trace(err)
	}
	err = inheritPlacementRuleFromDB(tbInfo, dbInfo)
	if err != nil {
		return ver, errors.Trace(err)
	}

	// build table & partition bundles if any.
	tableBundle, err := newBundleFromTblInfo(t, job, tbInfo)
	if err != nil {
		return ver, errors.Trace(err)
	}
	partitionBundles, err := newBundleFromPartition(t, job, tbInfo.Partition)
	if err != nil {
		return ver, errors.Trace(err)
	}
	bundles := make([]*placement.Bundle, 0, 1+len(partitionBundles))
	bundles = append(bundles, tableBundle)
	bundles = append(bundles, partitionBundles...)

	ver, err = updateSchemaVersion(t, job)
	if err != nil {
		return ver, errors.Trace(err)
	}

	switch tbInfo.State {
	case model.StateNone:
		// none -> public
		tbInfo.State = model.StatePublic
		tbInfo.UpdateTS = t.StartTS
		err = createTableOrViewWithCheck(t, job, schemaID, tbInfo)
		if err != nil {
			return ver, errors.Trace(err)
		}

		failpoint.Inject("checkOwnerCheckAllVersionsWaitTime", func(val failpoint.Value) {
			if val.(bool) {
				failpoint.Return(ver, errors.New("mock create table error"))
			}
		})
		// Send the placement bundle to PD.
		err = infosync.PutRuleBundles(context.TODO(), bundles)
		if err != nil {
			job.State = model.JobStateCancelled
			return ver, errors.Wrapf(err, "failed to notify PD the placement rules")
		}

		// Finish this job.
		job.FinishTableJob(model.JobStateDone, model.StatePublic, ver, tbInfo)
		asyncNotifyEvent(d, &util.Event{Tp: model.ActionCreateTable, TableInfo: tbInfo})
		return ver, nil
	default:
		return ver, ErrInvalidDDLState.GenWithStackByArgs("table", tbInfo.State)
	}
}

func inheritPlacementRuleFromDB(tbInfo *model.TableInfo, dbInfo *model.DBInfo) error {
	if tbInfo.DirectPlacementOpts == nil && tbInfo.PlacementPolicyRef == nil {
		if dbInfo.DirectPlacementOpts != nil {
			clone := *dbInfo.DirectPlacementOpts
			tbInfo.DirectPlacementOpts = &clone
		}
		if dbInfo.PlacementPolicyRef != nil {
			clone := *dbInfo.PlacementPolicyRef
			tbInfo.PlacementPolicyRef = &clone
		}
	}
	// Can not use both a placement policy and direct assignment. If you alter specify both in a CREATE TABLE or ALTER TABLE an error will be returned.
	if tbInfo.DirectPlacementOpts != nil && tbInfo.PlacementPolicyRef != nil {
		return ErrPlacementPolicyWithDirectOption.GenWithStackByArgs(tbInfo.PlacementPolicyRef.Name)
	}
	return nil
}

func newBundleFromTblInfo(t *meta.Meta, job *model.Job, tbInfo *model.TableInfo) (*placement.Bundle, error) {
	bundle, err := newBundleFromPolicyOrDirectOptions(t, job, tbInfo.PlacementPolicyRef, tbInfo.DirectPlacementOpts)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if bundle == nil {
		return nil, nil
	}
	ids := []int64{tbInfo.ID}
	// build the default partition rules in the table-level bundle.
	if tbInfo.Partition != nil {
		for _, pDef := range tbInfo.Partition.Definitions {
			ids = append(ids, pDef.ID)
		}
	}
	bundle.Reset(placement.RuleIndexTable, ids)
	return bundle, nil
}

func newBundleFromPartition(t *meta.Meta, job *model.Job, partition *model.PartitionInfo) ([]*placement.Bundle, error) {
	if partition == nil {
		return nil, nil
	}
	bundles := make([]*placement.Bundle, 0, len(partition.Definitions))
	// If the partition has the placement rules on their own, build the partition-level bundles additionally.
	for _, def := range partition.Definitions {
		bundle, err := newBundleFromPolicyOrDirectOptions(t, job, def.PlacementPolicyRef, def.DirectPlacementOpts)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if bundle == nil {
			continue
		}
		bundle.Reset(placement.RuleIndexPartition, []int64{def.ID})
		bundles = append(bundles, bundle)
		continue
	}
	return bundles, nil
}

func newBundleFromPolicyOrDirectOptions(t *meta.Meta, job *model.Job, ref *model.PolicyRefInfo, directOpts *model.PlacementSettings) (*placement.Bundle, error) {
	if directOpts != nil {
		// build bundle from direct placement options.
		bundle, err := placement.NewBundleFromOptions(directOpts)
		if err != nil {
			job.State = model.JobStateCancelled
			return nil, errors.Trace(err)
		}
		return bundle, nil
	}
	if ref != nil {
		// placement policy reference will override the direct placement options.
		po, err := checkPlacementPolicyExistAndCancelNonExistJob(t, job, ref.ID)
		if err != nil {
			job.State = model.JobStateCancelled
			return nil, errors.Trace(infoschema.ErrPlacementPolicyNotExists.GenWithStackByArgs(ref.Name))
		}
		// build bundle from placement policy.
		bundle, err := placement.NewBundleFromOptions(po.PlacementSettings)
		if err != nil {
			job.State = model.JobStateCancelled
			return nil, errors.Trace(err)
		}
		return bundle, nil
	}
	return nil, nil
}

func createTableOrViewWithCheck(t *meta.Meta, job *model.Job, schemaID int64, tbInfo *model.TableInfo) error {
	err := checkTableInfoValid(tbInfo)
	if err != nil {
		job.State = model.JobStateCancelled
		return errors.Trace(err)
	}
	return t.CreateTableOrView(schemaID, tbInfo)
}

func repairTableOrViewWithCheck(t *meta.Meta, job *model.Job, schemaID int64, tbInfo *model.TableInfo) error {
	err := checkTableInfoValid(tbInfo)
	if err != nil {
		job.State = model.JobStateCancelled
		return errors.Trace(err)
	}
	return t.UpdateTable(schemaID, tbInfo)
}

func onCreateView(d *ddlCtx, t *meta.Meta, job *model.Job) (ver int64, _ error) {
	schemaID := job.SchemaID
	tbInfo := &model.TableInfo{}
	var orReplace bool
	var oldTbInfoID int64
	if err := job.DecodeArgs(tbInfo, &orReplace, &oldTbInfoID); err != nil {
		// Invalid arguments, cancel this job.
		job.State = model.JobStateCancelled
		return ver, errors.Trace(err)
	}
	tbInfo.State = model.StateNone
	err := checkTableNotExists(d, t, schemaID, tbInfo.Name.L)
	if err != nil {
		if infoschema.ErrDatabaseNotExists.Equal(err) {
			job.State = model.JobStateCancelled
			return ver, errors.Trace(err)
		} else if infoschema.ErrTableExists.Equal(err) {
			if !orReplace {
				job.State = model.JobStateCancelled
				return ver, errors.Trace(err)
			}
		} else {
			return ver, errors.Trace(err)
		}
	}
	ver, err = updateSchemaVersion(t, job)
	if err != nil {
		return ver, errors.Trace(err)
	}
	switch tbInfo.State {
	case model.StateNone:
		// none -> public
		tbInfo.State = model.StatePublic
		tbInfo.UpdateTS = t.StartTS
		if oldTbInfoID > 0 && orReplace {
			err = t.DropTableOrView(schemaID, oldTbInfoID)
			if err != nil {
				return ver, errors.Trace(err)
			}
			err = t.GetAutoIDAccessors(schemaID, oldTbInfoID).Del()
			if err != nil {
				return ver, errors.Trace(err)
			}
		}
		err = createTableOrViewWithCheck(t, job, schemaID, tbInfo)
		if err != nil {
			return ver, errors.Trace(err)
		}
		// Finish this job.
		job.FinishTableJob(model.JobStateDone, model.StatePublic, ver, tbInfo)
		asyncNotifyEvent(d, &util.Event{Tp: model.ActionCreateView, TableInfo: tbInfo})
		return ver, nil
	default:
		return ver, ErrInvalidDDLState.GenWithStackByArgs("table", tbInfo.State)
	}
}

func onDropTableOrView(d *ddlCtx, t *meta.Meta, job *model.Job) (ver int64, _ error) {
	tblInfo, err := checkTableExistAndCancelNonExistJob(t, job, job.SchemaID)
	if err != nil {
		return ver, errors.Trace(err)
	}

	originalState := job.SchemaState
	switch tblInfo.State {
	case model.StatePublic:
		// public -> write only
		tblInfo.State = model.StateWriteOnly
		ver, err = updateVersionAndTableInfo(t, job, tblInfo, originalState != tblInfo.State)
		if err != nil {
			return ver, errors.Trace(err)
		}
		job.SchemaState = model.StateWriteOnly
	case model.StateWriteOnly:
		// write only -> delete only
		tblInfo.State = model.StateDeleteOnly
		ver, err = updateVersionAndTableInfo(t, job, tblInfo, originalState != tblInfo.State)
		if err != nil {
			return ver, errors.Trace(err)
		}
		job.SchemaState = model.StateDeleteOnly
	case model.StateDeleteOnly:
		tblInfo.State = model.StateNone
		oldIDs := getPartitionIDs(tblInfo)
		ruleIDs := append(getPartitionRuleIDs(job.SchemaName, tblInfo), fmt.Sprintf(label.TableIDFormat, label.IDPrefix, job.SchemaName, tblInfo.Name.L))
		job.CtxVars = []interface{}{oldIDs}

		ver, err = updateVersionAndTableInfo(t, job, tblInfo, originalState != tblInfo.State)
		if err != nil {
			return ver, errors.Trace(err)
		}
		if tblInfo.IsSequence() {
			if err = t.DropSequence(job.SchemaID, job.TableID); err != nil {
				break
			}
		} else {
			if err = t.DropTableOrView(job.SchemaID, job.TableID); err != nil {
				break
			}
			if err = t.GetAutoIDAccessors(job.SchemaID, job.TableID).Del(); err != nil {
				break
			}
		}
		// Placement rules cannot be removed immediately after drop table / truncate table, because the
		// tables can be flashed back or recovered, therefore it moved to doGCPlacementRules in gc_worker.go.

		// Finish this job.
		job.FinishTableJob(model.JobStateDone, model.StateNone, ver, tblInfo)
		startKey := tablecodec.EncodeTablePrefix(job.TableID)
		job.Args = append(job.Args, startKey, oldIDs, ruleIDs)
	default:
		err = ErrInvalidDDLState.GenWithStackByArgs("table", tblInfo.State)
	}

	return ver, errors.Trace(err)
}

const (
	recoverTableCheckFlagNone int64 = iota
	recoverTableCheckFlagEnableGC
	recoverTableCheckFlagDisableGC
)

func (w *worker) onRecoverTable(d *ddlCtx, t *meta.Meta, job *model.Job) (ver int64, err error) {
	schemaID := job.SchemaID
	tblInfo := &model.TableInfo{}
	var autoIncID, autoRandID, dropJobID, recoverTableCheckFlag int64
	var snapshotTS uint64
	var oldTableName, oldSchemaName string
	const checkFlagIndexInJobArgs = 4 // The index of `recoverTableCheckFlag` in job arg list.
	if err = job.DecodeArgs(tblInfo, &autoIncID, &dropJobID, &snapshotTS, &recoverTableCheckFlag, &autoRandID, &oldSchemaName, &oldTableName); err != nil {
		// Invalid arguments, cancel this job.
		job.State = model.JobStateCancelled
		return ver, errors.Trace(err)
	}

	// check GC and safe point
	gcEnable, err := checkGCEnable(w)
	if err != nil {
		job.State = model.JobStateCancelled
		return ver, errors.Trace(err)
	}

	err = checkTableNotExists(d, t, schemaID, tblInfo.Name.L)
	if err != nil {
		if infoschema.ErrDatabaseNotExists.Equal(err) || infoschema.ErrTableExists.Equal(err) {
			job.State = model.JobStateCancelled
		}
		return ver, errors.Trace(err)
	}

	err = checkTableIDNotExists(t, schemaID, tblInfo.ID)
	if err != nil {
		if infoschema.ErrDatabaseNotExists.Equal(err) || infoschema.ErrTableExists.Equal(err) {
			job.State = model.JobStateCancelled
		}
		return ver, errors.Trace(err)
	}

	// Recover table divide into 2 steps:
	// 1. Check GC enable status, to decided whether enable GC after recover table.
	//     a. Why not disable GC before put the job to DDL job queue?
	//        Think about concurrency problem. If a recover job-1 is doing and already disabled GC,
	//        then, another recover table job-2 check GC enable will get disable before into the job queue.
	//        then, after recover table job-2 finished, the GC will be disabled.
	//     b. Why split into 2 steps? 1 step also can finish this job: check GC -> disable GC -> recover table -> finish job.
	//        What if the transaction commit failed? then, the job will retry, but the GC already disabled when first running.
	//        So, after this job retry succeed, the GC will be disabled.
	// 2. Do recover table job.
	//     a. Check whether GC enabled, if enabled, disable GC first.
	//     b. Check GC safe point. If drop table time if after safe point time, then can do recover.
	//        otherwise, can't recover table, because the records of the table may already delete by gc.
	//     c. Remove GC task of the table from gc_delete_range table.
	//     d. Create table and rebase table auto ID.
	//     e. Finish.
	switch tblInfo.State {
	case model.StateNone:
		// none -> write only
		// check GC enable and update flag.
		if gcEnable {
			job.Args[checkFlagIndexInJobArgs] = recoverTableCheckFlagEnableGC
		} else {
			job.Args[checkFlagIndexInJobArgs] = recoverTableCheckFlagDisableGC
		}

		job.SchemaState = model.StateWriteOnly
		tblInfo.State = model.StateWriteOnly
		ver, err = updateVersionAndTableInfo(t, job, tblInfo, false)
		if err != nil {
			return ver, errors.Trace(err)
		}
	case model.StateWriteOnly:
		// write only -> public
		// do recover table.
		if gcEnable {
			err = disableGC(w)
			if err != nil {
				job.State = model.JobStateCancelled
				return ver, errors.Errorf("disable gc failed, try again later. err: %v", err)
			}
		}
		// check GC safe point
		err = checkSafePoint(w, snapshotTS)
		if err != nil {
			job.State = model.JobStateCancelled
			return ver, errors.Trace(err)
		}
		// Remove dropped table DDL job from gc_delete_range table.
		var tids []int64
		if tblInfo.GetPartitionInfo() != nil {
			tids = getPartitionIDs(tblInfo)
		} else {
			tids = []int64{tblInfo.ID}
		}

		tableRuleID, partRuleIDs, oldRuleIDs, oldRules, err := getOldLabelRules(tblInfo, oldSchemaName, oldTableName)
		if err != nil {
			job.State = model.JobStateCancelled
			return ver, errors.Wrapf(err, "failed to get old label rules from PD")
		}

		err = w.delRangeManager.removeFromGCDeleteRange(w.ddlJobCtx, dropJobID, tids)
		if err != nil {
			return ver, errors.Trace(err)
		}

		tblInfo.State = model.StatePublic
		tblInfo.UpdateTS = t.StartTS
		err = t.CreateTableAndSetAutoID(schemaID, tblInfo, autoIncID, autoRandID)
		if err != nil {
			return ver, errors.Trace(err)
		}

		failpoint.Inject("mockRecoverTableCommitErr", func(val failpoint.Value) {
			if val.(bool) && atomic.CompareAndSwapUint32(&mockRecoverTableCommitErrOnce, 0, 1) {
				err = failpoint.Enable(`tikvclient/mockCommitErrorOpt`, "return(true)")
			}
		})

		err = updateLabelRules(job, tblInfo, oldRules, tableRuleID, partRuleIDs, oldRuleIDs, tblInfo.ID)
		if err != nil {
			job.State = model.JobStateCancelled
			return ver, errors.Wrapf(err, "failed to update the label rule to PD")
		}

		job.CtxVars = []interface{}{tids}
		ver, err = updateVersionAndTableInfo(t, job, tblInfo, true)
		if err != nil {
			return ver, errors.Trace(err)
		}

		// Finish this job.
		job.FinishTableJob(model.JobStateDone, model.StatePublic, ver, tblInfo)
	default:
		return ver, ErrInvalidDDLState.GenWithStackByArgs("table", tblInfo.State)
	}
	return ver, nil
}

// mockRecoverTableCommitErrOnce uses to make sure
// `mockRecoverTableCommitErr` only mock error once.
var mockRecoverTableCommitErrOnce uint32

func enableGC(w *worker) error {
	ctx, err := w.sessPool.get()
	if err != nil {
		return errors.Trace(err)
	}
	defer w.sessPool.put(ctx)

	return gcutil.EnableGC(ctx)
}

func disableGC(w *worker) error {
	ctx, err := w.sessPool.get()
	if err != nil {
		return errors.Trace(err)
	}
	defer w.sessPool.put(ctx)

	return gcutil.DisableGC(ctx)
}

func checkGCEnable(w *worker) (enable bool, err error) {
	ctx, err := w.sessPool.get()
	if err != nil {
		return false, errors.Trace(err)
	}
	defer w.sessPool.put(ctx)

	return gcutil.CheckGCEnable(ctx)
}

func checkSafePoint(w *worker, snapshotTS uint64) error {
	ctx, err := w.sessPool.get()
	if err != nil {
		return errors.Trace(err)
	}
	defer w.sessPool.put(ctx)

	return gcutil.ValidateSnapshot(ctx, snapshotTS)
}

func getTable(store kv.Storage, schemaID int64, tblInfo *model.TableInfo) (table.Table, error) {
	allocs := autoid.NewAllocatorsFromTblInfo(store, schemaID, tblInfo)
	tbl, err := table.TableFromMeta(allocs, tblInfo)
	return tbl, errors.Trace(err)
}

func getTableInfoAndCancelFaultJob(t *meta.Meta, job *model.Job, schemaID int64) (*model.TableInfo, error) {
	tblInfo, err := checkTableExistAndCancelNonExistJob(t, job, schemaID)
	if err != nil {
		return nil, errors.Trace(err)
	}

	if tblInfo.State != model.StatePublic {
		job.State = model.JobStateCancelled
		return nil, ErrInvalidDDLState.GenWithStack("table %s is not in public, but %s", tblInfo.Name, tblInfo.State)
	}

	return tblInfo, nil
}

func checkTableExistAndCancelNonExistJob(t *meta.Meta, job *model.Job, schemaID int64) (*model.TableInfo, error) {
	tblInfo, err := getTableInfo(t, job.TableID, schemaID)
	if err == nil {
		return tblInfo, nil
	}
	if infoschema.ErrDatabaseNotExists.Equal(err) || infoschema.ErrTableNotExists.Equal(err) {
		job.State = model.JobStateCancelled
	}
	return nil, err
}

func getTableInfo(t *meta.Meta, tableID, schemaID int64) (*model.TableInfo, error) {
	// Check this table's database.
	tblInfo, err := t.GetTable(schemaID, tableID)
	if err != nil {
		if meta.ErrDBNotExists.Equal(err) {
			return nil, errors.Trace(infoschema.ErrDatabaseNotExists.GenWithStackByArgs(
				fmt.Sprintf("(Schema ID %d)", schemaID),
			))
		}
		return nil, errors.Trace(err)
	}

	// Check the table.
	if tblInfo == nil {
		return nil, errors.Trace(infoschema.ErrTableNotExists.GenWithStackByArgs(
			fmt.Sprintf("(Schema ID %d)", schemaID),
			fmt.Sprintf("(Table ID %d)", tableID),
		))
	}
	return tblInfo, nil
}

// onTruncateTable delete old table meta, and creates a new table identical to old table except for table ID.
// As all the old data is encoded with old table ID, it can not be accessed any more.
// A background job will be created to delete old data.
func onTruncateTable(d *ddlCtx, t *meta.Meta, job *model.Job) (ver int64, _ error) {
	schemaID := job.SchemaID
	tableID := job.TableID
	var newTableID int64
	err := job.DecodeArgs(&newTableID)
	if err != nil {
		job.State = model.JobStateCancelled
		return ver, errors.Trace(err)
	}
	tblInfo, err := getTableInfoAndCancelFaultJob(t, job, schemaID)
	if err != nil {
		return ver, errors.Trace(err)
	}
	if tblInfo.IsView() || tblInfo.IsSequence() {
		job.State = model.JobStateCancelled
		return ver, infoschema.ErrTableNotExists.GenWithStackByArgs(job.SchemaName, tblInfo.Name.O)
	}

	err = t.DropTableOrView(schemaID, tblInfo.ID)
	if err != nil {
		job.State = model.JobStateCancelled
		return ver, errors.Trace(err)
	}
	err = t.GetAutoIDAccessors(schemaID, tblInfo.ID).Del()
	if err != nil {
		job.State = model.JobStateCancelled
		return ver, errors.Trace(err)
	}
	failpoint.Inject("truncateTableErr", func(val failpoint.Value) {
		if val.(bool) {
			job.State = model.JobStateCancelled
			failpoint.Return(ver, errors.New("occur an error after dropping table"))
		}
	})

	var oldPartitionIDs []int64
	if tblInfo.GetPartitionInfo() != nil {
		oldPartitionIDs = getPartitionIDs(tblInfo)
		// We use the new partition ID because all the old data is encoded with the old partition ID, it can not be accessed anymore.
		err = truncateTableByReassignPartitionIDs(t, tblInfo)
		if err != nil {
			return ver, errors.Trace(err)
		}
	}

	is := d.infoCache.GetLatest()

	bundles := make([]*placement.Bundle, 0, len(oldPartitionIDs)+1)
	if oldBundle, ok := is.BundleByName(placement.GroupID(tableID)); ok {
		bundles = append(bundles, oldBundle.Clone().Reset(placement.RuleIndexTable, []int64{newTableID}))
	}

	if pi := tblInfo.GetPartitionInfo(); pi != nil {
		oldIDs := make([]int64, 0, len(oldPartitionIDs))
		newIDs := make([]int64, 0, len(oldPartitionIDs))
		newDefs := pi.Definitions
		for i := range oldPartitionIDs {
			newID := newDefs[i].ID
			if oldBundle, ok := is.BundleByName(placement.GroupID(oldPartitionIDs[i])); ok && !oldBundle.IsEmpty() {
				oldIDs = append(oldIDs, oldPartitionIDs[i])
				newIDs = append(newIDs, newID)
				bundles = append(bundles, oldBundle.Clone().Reset(placement.RuleIndexPartition, []int64{newID}))
			}
		}
		job.CtxVars = []interface{}{oldIDs, newIDs}
	}

	err = infosync.PutRuleBundles(context.TODO(), bundles)
	if err != nil {
		job.State = model.JobStateCancelled
		return 0, errors.Wrapf(err, "failed to notify PD the placement rules")
	}

	tableRuleID, partRuleIDs, _, oldRules, err := getOldLabelRules(tblInfo, job.SchemaName, tblInfo.Name.L)
	if err != nil {
		job.State = model.JobStateCancelled
		return 0, errors.Wrapf(err, "failed to get old label rules from PD")
	}

	err = updateLabelRules(job, tblInfo, oldRules, tableRuleID, partRuleIDs, []string{}, newTableID)
	if err != nil {
		job.State = model.JobStateCancelled
		return 0, errors.Wrapf(err, "failed to update the label rule to PD")
	}

	// Clear the tiflash replica available status.
	if tblInfo.TiFlashReplica != nil {
		tblInfo.TiFlashReplica.AvailablePartitionIDs = nil
		tblInfo.TiFlashReplica.Available = false
	}

	tblInfo.ID = newTableID
	err = t.CreateTableOrView(schemaID, tblInfo)
	if err != nil {
		job.State = model.JobStateCancelled
		return ver, errors.Trace(err)
	}

	failpoint.Inject("mockTruncateTableUpdateVersionError", func(val failpoint.Value) {
		if val.(bool) {
			failpoint.Return(ver, errors.New("mock update version error"))
		}
	})

	ver, err = updateSchemaVersion(t, job)
	if err != nil {
		return ver, errors.Trace(err)
	}
	job.FinishTableJob(model.JobStateDone, model.StatePublic, ver, tblInfo)
	asyncNotifyEvent(d, &util.Event{Tp: model.ActionTruncateTable, TableInfo: tblInfo})
	startKey := tablecodec.EncodeTablePrefix(tableID)
	job.Args = []interface{}{startKey, oldPartitionIDs}
	return ver, nil
}

func onRebaseRowIDType(store kv.Storage, t *meta.Meta, job *model.Job) (ver int64, _ error) {
	return onRebaseAutoID(store, t, job, autoid.RowIDAllocType)
}

func onRebaseAutoRandomType(store kv.Storage, t *meta.Meta, job *model.Job) (ver int64, _ error) {
	return onRebaseAutoID(store, t, job, autoid.AutoRandomType)
}

func onRebaseAutoID(store kv.Storage, t *meta.Meta, job *model.Job, tp autoid.AllocatorType) (ver int64, _ error) {
	schemaID := job.SchemaID
	var (
		newBase int64
		force   bool
	)
	err := job.DecodeArgs(&newBase, &force)
	if err != nil {
		job.State = model.JobStateCancelled
		return ver, errors.Trace(err)
	}
	tblInfo, err := getTableInfoAndCancelFaultJob(t, job, schemaID)
	if err != nil {
		job.State = model.JobStateCancelled
		return ver, errors.Trace(err)
	}
	// No need to check `newBase` again, because `RebaseAutoID` will do this check.
	if tp == autoid.RowIDAllocType {
		tblInfo.AutoIncID = newBase
	} else {
		tblInfo.AutoRandID = newBase
	}

	tbl, err := getTable(store, schemaID, tblInfo)
	if err != nil {
		job.State = model.JobStateCancelled
		return ver, errors.Trace(err)
	}
	if alloc := tbl.Allocators(nil).Get(tp); alloc != nil {
		// The next value to allocate is `newBase`.
		newEnd := newBase - 1
		if force {
			err = alloc.ForceRebase(newEnd)
		} else {
			err = alloc.Rebase(context.Background(), newEnd, false)
		}
		if err != nil {
			job.State = model.JobStateCancelled
			return ver, errors.Trace(err)
		}
	}
	ver, err = updateVersionAndTableInfo(t, job, tblInfo, true)
	if err != nil {
		return ver, errors.Trace(err)
	}
	job.FinishTableJob(model.JobStateDone, model.StatePublic, ver, tblInfo)
	return ver, nil
}

func onModifyTableAutoIDCache(t *meta.Meta, job *model.Job) (int64, error) {
	var cache int64
	if err := job.DecodeArgs(&cache); err != nil {
		job.State = model.JobStateCancelled
		return 0, errors.Trace(err)
	}

	tblInfo, err := getTableInfoAndCancelFaultJob(t, job, job.SchemaID)
	if err != nil {
		return 0, errors.Trace(err)
	}

	tblInfo.AutoIdCache = cache
	ver, err := updateVersionAndTableInfo(t, job, tblInfo, true)
	if err != nil {
		return ver, errors.Trace(err)
	}
	job.FinishTableJob(model.JobStateDone, model.StatePublic, ver, tblInfo)
	return ver, nil
}

func (w *worker) onShardRowID(d *ddlCtx, t *meta.Meta, job *model.Job) (ver int64, _ error) {
	var shardRowIDBits uint64
	err := job.DecodeArgs(&shardRowIDBits)
	if err != nil {
		job.State = model.JobStateCancelled
		return ver, errors.Trace(err)
	}
	tblInfo, err := getTableInfoAndCancelFaultJob(t, job, job.SchemaID)
	if err != nil {
		job.State = model.JobStateCancelled
		return ver, errors.Trace(err)
	}
	if shardRowIDBits < tblInfo.ShardRowIDBits {
		tblInfo.ShardRowIDBits = shardRowIDBits
	} else {
		tbl, err := getTable(d.store, job.SchemaID, tblInfo)
		if err != nil {
			return ver, errors.Trace(err)
		}
		err = verifyNoOverflowShardBits(w.sessPool, tbl, shardRowIDBits)
		if err != nil {
			job.State = model.JobStateCancelled
			return ver, err
		}
		tblInfo.ShardRowIDBits = shardRowIDBits
		// MaxShardRowIDBits use to check the overflow of auto ID.
		tblInfo.MaxShardRowIDBits = shardRowIDBits
	}
	ver, err = updateVersionAndTableInfo(t, job, tblInfo, true)
	if err != nil {
		job.State = model.JobStateCancelled
		return ver, errors.Trace(err)
	}
	job.FinishTableJob(model.JobStateDone, model.StatePublic, ver, tblInfo)
	return ver, nil
}

func verifyNoOverflowShardBits(s *sessionPool, tbl table.Table, shardRowIDBits uint64) error {
	ctx, err := s.get()
	if err != nil {
		return errors.Trace(err)
	}
	defer s.put(ctx)

	// Check next global max auto ID first.
	autoIncID, err := tbl.Allocators(ctx).Get(autoid.RowIDAllocType).NextGlobalAutoID()
	if err != nil {
		return errors.Trace(err)
	}
	if tables.OverflowShardBits(autoIncID, shardRowIDBits, autoid.RowIDBitLength, true) {
		return autoid.ErrAutoincReadFailed.GenWithStack("shard_row_id_bits %d will cause next global auto ID %v overflow", shardRowIDBits, autoIncID)
	}
	return nil
}

func onRenameTable(d *ddlCtx, t *meta.Meta, job *model.Job) (ver int64, _ error) {
	var oldSchemaID int64
	var oldSchemaName model.CIStr
	var tableName model.CIStr
	if err := job.DecodeArgs(&oldSchemaID, &tableName, &oldSchemaName); err != nil {
		// Invalid arguments, cancel this job.
		job.State = model.JobStateCancelled
		return ver, errors.Trace(err)
	}

	newSchemaID := job.SchemaID
	err := checkTableNotExists(d, t, newSchemaID, tableName.L)
	if err != nil {
		if infoschema.ErrDatabaseNotExists.Equal(err) || infoschema.ErrTableExists.Equal(err) {
			job.State = model.JobStateCancelled
		}
		return ver, errors.Trace(err)
	}

	ver, tblInfo, err := checkAndRenameTables(t, job, oldSchemaID, job.SchemaID, &oldSchemaName, &tableName)
	if err != nil {
		return ver, errors.Trace(err)
	}

	ver, err = updateSchemaVersion(t, job)
	if err != nil {
		return ver, errors.Trace(err)
	}
	job.FinishTableJob(model.JobStateDone, model.StatePublic, ver, tblInfo)
	return ver, nil
}

func onRenameTables(d *ddlCtx, t *meta.Meta, job *model.Job) (ver int64, _ error) {
	oldSchemaIDs := []int64{}
	newSchemaIDs := []int64{}
	tableNames := []*model.CIStr{}
	tableIDs := []int64{}
	oldSchemaNames := []*model.CIStr{}
	if err := job.DecodeArgs(&oldSchemaIDs, &newSchemaIDs, &tableNames, &tableIDs, &oldSchemaNames); err != nil {
		job.State = model.JobStateCancelled
		return ver, errors.Trace(err)
	}

	tblInfo := &model.TableInfo{}
	var err error
	for i, oldSchemaID := range oldSchemaIDs {
		job.TableID = tableIDs[i]
		ver, tblInfo, err = checkAndRenameTables(t, job, oldSchemaID, newSchemaIDs[i], oldSchemaNames[i], tableNames[i])
		if err != nil {
			return ver, errors.Trace(err)
		}
	}

	ver, err = updateSchemaVersion(t, job)
	if err != nil {
		return ver, errors.Trace(err)
	}
	job.FinishTableJob(model.JobStateDone, model.StatePublic, ver, tblInfo)
	return ver, nil
}

func checkAndRenameTables(t *meta.Meta, job *model.Job, oldSchemaID, newSchemaID int64, oldSchemaName, tableName *model.CIStr) (ver int64, tblInfo *model.TableInfo, _ error) {
	tblInfo, err := getTableInfoAndCancelFaultJob(t, job, oldSchemaID)
	if err != nil {
		return ver, tblInfo, errors.Trace(err)
	}

	err = t.DropTableOrView(oldSchemaID, tblInfo.ID)
	if err != nil {
		job.State = model.JobStateCancelled
		return ver, tblInfo, errors.Trace(err)
	}

	failpoint.Inject("renameTableErr", func(val failpoint.Value) {
		if valStr, ok := val.(string); ok {
			if tableName.L == valStr {
				job.State = model.JobStateCancelled
				failpoint.Return(ver, tblInfo, errors.New("occur an error after renaming table"))
			}
		}
	})

	oldTableName := tblInfo.Name
	tableRuleID, partRuleIDs, oldRuleIDs, oldRules, err := getOldLabelRules(tblInfo, oldSchemaName.L, oldTableName.L)
	if err != nil {
		job.State = model.JobStateCancelled
		return ver, tblInfo, errors.Wrapf(err, "failed to get old label rules from PD")
	}

	tblInfo.Name = *tableName
	err = t.CreateTableOrView(newSchemaID, tblInfo)
	if err != nil {
		job.State = model.JobStateCancelled
		return ver, tblInfo, errors.Trace(err)
	}

	if newSchemaID != oldSchemaID {
		oldDBID := tblInfo.GetDBID(oldSchemaID)
		err := meta.BackupAndRestoreAutoIDs(t, oldDBID, tblInfo.ID, newSchemaID, tblInfo.ID)
		if err != nil {
			job.State = model.JobStateCancelled
			return ver, tblInfo, errors.Trace(err)
		}
		// It's compatible with old version.
		// TODO: Remove it.
		tblInfo.OldSchemaID = 0
	}

	err = updateLabelRules(job, tblInfo, oldRules, tableRuleID, partRuleIDs, oldRuleIDs, tblInfo.ID)
	if err != nil {
		job.State = model.JobStateCancelled
		return ver, tblInfo, errors.Wrapf(err, "failed to update the label rule to PD")
	}

	return ver, tblInfo, nil
}

func onModifyTableComment(t *meta.Meta, job *model.Job) (ver int64, _ error) {
	var comment string
	if err := job.DecodeArgs(&comment); err != nil {
		job.State = model.JobStateCancelled
		return ver, errors.Trace(err)
	}

	tblInfo, err := getTableInfoAndCancelFaultJob(t, job, job.SchemaID)
	if err != nil {
		return ver, errors.Trace(err)
	}

	tblInfo.Comment = comment
	ver, err = updateVersionAndTableInfo(t, job, tblInfo, true)
	if err != nil {
		return ver, errors.Trace(err)
	}
	job.FinishTableJob(model.JobStateDone, model.StatePublic, ver, tblInfo)
	return ver, nil
}

func onModifyTableCharsetAndCollate(t *meta.Meta, job *model.Job) (ver int64, _ error) {
	var toCharset, toCollate string
	var needsOverwriteCols bool
	if err := job.DecodeArgs(&toCharset, &toCollate, &needsOverwriteCols); err != nil {
		job.State = model.JobStateCancelled
		return ver, errors.Trace(err)
	}

	dbInfo, err := checkSchemaExistAndCancelNotExistJob(t, job)
	if err != nil {
		return ver, errors.Trace(err)
	}

	tblInfo, err := getTableInfoAndCancelFaultJob(t, job, job.SchemaID)
	if err != nil {
		return ver, errors.Trace(err)
	}

	// double check.
	_, err = checkAlterTableCharset(tblInfo, dbInfo, toCharset, toCollate, needsOverwriteCols)
	if err != nil {
		job.State = model.JobStateCancelled
		return ver, errors.Trace(err)
	}

	tblInfo.Charset = toCharset
	tblInfo.Collate = toCollate

	if needsOverwriteCols {
		// update column charset.
		for _, col := range tblInfo.Columns {
			if field_types.HasCharset(&col.FieldType) {
				col.Charset = toCharset
				col.Collate = toCollate
			} else {
				col.Charset = charset.CharsetBin
				col.Collate = charset.CharsetBin
			}
		}
	}

	ver, err = updateVersionAndTableInfo(t, job, tblInfo, true)
	if err != nil {
		return ver, errors.Trace(err)
	}
	job.FinishTableJob(model.JobStateDone, model.StatePublic, ver, tblInfo)
	return ver, nil
}

func (w *worker) onSetTableFlashReplica(t *meta.Meta, job *model.Job) (ver int64, _ error) {
	var replicaInfo ast.TiFlashReplicaSpec
	if err := job.DecodeArgs(&replicaInfo); err != nil {
		job.State = model.JobStateCancelled
		return ver, errors.Trace(err)
	}

	tblInfo, err := getTableInfoAndCancelFaultJob(t, job, job.SchemaID)
	if err != nil {
		return ver, errors.Trace(err)
	}

	// Ban setting replica count for tables in system database.
	if tidb_util.IsMemOrSysDB(job.SchemaName) {
		return ver, errors.Trace(errUnsupportedAlterReplicaForSysTable)
	}

	err = w.checkTiFlashReplicaCount(replicaInfo.Count)
	if err != nil {
		job.State = model.JobStateCancelled
		return ver, errors.Trace(err)
	}

	if replicaInfo.Count > 0 {
		tblInfo.TiFlashReplica = &model.TiFlashReplicaInfo{
			Count:          replicaInfo.Count,
			LocationLabels: replicaInfo.Labels,
		}
	} else {
		tblInfo.TiFlashReplica = nil
	}

	ver, err = updateVersionAndTableInfo(t, job, tblInfo, true)
	if err != nil {
		return ver, errors.Trace(err)
	}
	job.FinishTableJob(model.JobStateDone, model.StatePublic, ver, tblInfo)
	return ver, nil
}

func (w *worker) checkTiFlashReplicaCount(replicaCount uint64) error {
	ctx, err := w.sessPool.get()
	if err != nil {
		return errors.Trace(err)
	}
	defer w.sessPool.put(ctx)

	return checkTiFlashReplicaCount(ctx, replicaCount)
}

func onUpdateFlashReplicaStatus(t *meta.Meta, job *model.Job) (ver int64, _ error) {
	var available bool
	var physicalID int64
	if err := job.DecodeArgs(&available, &physicalID); err != nil {
		job.State = model.JobStateCancelled
		return ver, errors.Trace(err)
	}

	tblInfo, err := getTableInfoAndCancelFaultJob(t, job, job.SchemaID)
	if err != nil {
		return ver, errors.Trace(err)
	}
	if tblInfo.TiFlashReplica == nil || (tblInfo.ID == physicalID && tblInfo.TiFlashReplica.Available == available) ||
		(tblInfo.ID != physicalID && available == tblInfo.TiFlashReplica.IsPartitionAvailable(physicalID)) {
		job.State = model.JobStateCancelled
		return ver, errors.Errorf("the replica available status of table %s is already updated", tblInfo.Name.String())
	}

	if tblInfo.ID == physicalID {
		tblInfo.TiFlashReplica.Available = available
	} else if pi := tblInfo.GetPartitionInfo(); pi != nil {
		// Partition replica become available.
		if available {
			allAvailable := true
			for _, p := range pi.Definitions {
				if p.ID == physicalID {
					tblInfo.TiFlashReplica.AvailablePartitionIDs = append(tblInfo.TiFlashReplica.AvailablePartitionIDs, physicalID)
				}
				allAvailable = allAvailable && tblInfo.TiFlashReplica.IsPartitionAvailable(p.ID)
			}
			tblInfo.TiFlashReplica.Available = allAvailable
		} else {
			// Partition replica become unavailable.
			for i, id := range tblInfo.TiFlashReplica.AvailablePartitionIDs {
				if id == physicalID {
					newIDs := tblInfo.TiFlashReplica.AvailablePartitionIDs[:i]
					newIDs = append(newIDs, tblInfo.TiFlashReplica.AvailablePartitionIDs[i+1:]...)
					tblInfo.TiFlashReplica.AvailablePartitionIDs = newIDs
					tblInfo.TiFlashReplica.Available = false
					break
				}
			}
		}
	} else {
		job.State = model.JobStateCancelled
		return ver, errors.Errorf("unknown physical ID %v in table %v", physicalID, tblInfo.Name.O)
	}

	ver, err = updateVersionAndTableInfo(t, job, tblInfo, true)
	if err != nil {
		return ver, errors.Trace(err)
	}
	job.FinishTableJob(model.JobStateDone, model.StatePublic, ver, tblInfo)
	return ver, nil
}

func checkTableNotExists(d *ddlCtx, t *meta.Meta, schemaID int64, tableName string) error {
	// Try to use memory schema info to check first.
	currVer, err := t.GetSchemaVersion()
	if err != nil {
		return err
	}
	is := d.infoCache.GetLatest()
	if is.SchemaMetaVersion() == currVer {
		return checkTableNotExistsFromInfoSchema(is, schemaID, tableName)
	}

	return checkTableNotExistsFromStore(t, schemaID, tableName)
}

func checkTableIDNotExists(t *meta.Meta, schemaID, tableID int64) error {
	tbl, err := t.GetTable(schemaID, tableID)
	if err != nil {
		if meta.ErrDBNotExists.Equal(err) {
			return infoschema.ErrDatabaseNotExists.GenWithStackByArgs("")
		}
		return errors.Trace(err)
	}
	if tbl != nil {
		return infoschema.ErrTableExists.GenWithStackByArgs(tbl.Name)
	}
	return nil
}

func checkTableNotExistsFromInfoSchema(is infoschema.InfoSchema, schemaID int64, tableName string) error {
	// Check this table's database.
	schema, ok := is.SchemaByID(schemaID)
	if !ok {
		return infoschema.ErrDatabaseNotExists.GenWithStackByArgs("")
	}
	if is.TableExists(schema.Name, model.NewCIStr(tableName)) {
		return infoschema.ErrTableExists.GenWithStackByArgs(tableName)
	}
	return nil
}

func checkTableNotExistsFromStore(t *meta.Meta, schemaID int64, tableName string) error {
	// Check this table's database.
	tables, err := t.ListTables(schemaID)
	if err != nil {
		if meta.ErrDBNotExists.Equal(err) {
			return infoschema.ErrDatabaseNotExists.GenWithStackByArgs("")
		}
		return errors.Trace(err)
	}

	// Check the table.
	for _, tbl := range tables {
		if tbl.Name.L == tableName {
			return infoschema.ErrTableExists.GenWithStackByArgs(tbl.Name)
		}
	}

	return nil
}

// updateVersionAndTableInfoWithCheck checks table info validate and updates the schema version and the table information
func updateVersionAndTableInfoWithCheck(t *meta.Meta, job *model.Job, tblInfo *model.TableInfo, shouldUpdateVer bool) (
	ver int64, err error) {
	err = checkTableInfoValid(tblInfo)
	if err != nil {
		job.State = model.JobStateCancelled
		return ver, errors.Trace(err)
	}
	return updateVersionAndTableInfo(t, job, tblInfo, shouldUpdateVer)
}

// updateVersionAndTableInfo updates the schema version and the table information.
func updateVersionAndTableInfo(t *meta.Meta, job *model.Job, tblInfo *model.TableInfo, shouldUpdateVer bool) (
	ver int64, err error) {
	failpoint.Inject("mockUpdateVersionAndTableInfoErr", func(val failpoint.Value) {
		switch val.(int) {
		case 1:
			failpoint.Return(ver, errors.New("mock update version and tableInfo error"))
		case 2:
			// We change it cancelled directly here, because we want to get the original error with the job id appended.
			// The job ID will be used to get the job from history queue and we will assert it's args.
			job.State = model.JobStateCancelled
			failpoint.Return(ver, errors.New("mock update version and tableInfo error, jobID="+strconv.Itoa(int(job.ID))))
		default:
		}
	})
	if shouldUpdateVer {
		ver, err = updateSchemaVersion(t, job)
		if err != nil {
			return 0, errors.Trace(err)
		}
	}

	if tblInfo.State == model.StatePublic {
		tblInfo.UpdateTS = t.StartTS
	}
	return ver, t.UpdateTable(job.SchemaID, tblInfo)
}

func onRepairTable(d *ddlCtx, t *meta.Meta, job *model.Job) (ver int64, _ error) {
	schemaID := job.SchemaID
	tblInfo := &model.TableInfo{}

	if err := job.DecodeArgs(tblInfo); err != nil {
		// Invalid arguments, cancel this job.
		job.State = model.JobStateCancelled
		return ver, errors.Trace(err)
	}

	tblInfo.State = model.StateNone

	// Check the old DB and old table exist.
	_, err := getTableInfoAndCancelFaultJob(t, job, schemaID)
	if err != nil {
		return ver, errors.Trace(err)
	}

	// When in repair mode, the repaired table in a server is not access to user,
	// the table after repairing will be removed from repair list. Other server left
	// behind alive may need to restart to get the latest schema version.
	ver, err = updateSchemaVersion(t, job)
	if err != nil {
		return ver, errors.Trace(err)
	}
	switch tblInfo.State {
	case model.StateNone:
		// none -> public
		tblInfo.State = model.StatePublic
		tblInfo.UpdateTS = t.StartTS
		err = repairTableOrViewWithCheck(t, job, schemaID, tblInfo)
		if err != nil {
			return ver, errors.Trace(err)
		}
		// Finish this job.
		job.FinishTableJob(model.JobStateDone, model.StatePublic, ver, tblInfo)
		asyncNotifyEvent(d, &util.Event{Tp: model.ActionRepairTable, TableInfo: tblInfo})
		return ver, nil
	default:
		return ver, ErrInvalidDDLState.GenWithStackByArgs("table", tblInfo.State)
	}
}

func onAlterTableAttributes(t *meta.Meta, job *model.Job) (ver int64, err error) {
	rule := label.NewRule()
	err = job.DecodeArgs(rule)
	if err != nil {
		job.State = model.JobStateCancelled
		return 0, errors.Trace(err)
	}

	tblInfo, err := getTableInfoAndCancelFaultJob(t, job, job.SchemaID)
	if err != nil {
		return 0, err
	}

	if len(rule.Labels) == 0 {
		patch := label.NewRulePatch([]*label.Rule{}, []string{rule.ID})
		err = infosync.UpdateLabelRules(context.TODO(), patch)
	} else {
		err = infosync.PutLabelRule(context.TODO(), rule)
	}
	if err != nil {
		job.State = model.JobStateCancelled
		return 0, errors.Wrapf(err, "failed to notify PD label rule")
	}
	ver, err = updateVersionAndTableInfo(t, job, tblInfo, true)
	if err != nil {
		return ver, errors.Trace(err)
	}
	job.FinishTableJob(model.JobStateDone, model.StatePublic, ver, tblInfo)

	return ver, nil
}

func onAlterTablePartitionAttributes(t *meta.Meta, job *model.Job) (ver int64, err error) {
	var partitionID int64
	rule := label.NewRule()
	err = job.DecodeArgs(&partitionID, rule)
	if err != nil {
		job.State = model.JobStateCancelled
		return 0, errors.Trace(err)
	}
	tblInfo, err := getTableInfoAndCancelFaultJob(t, job, job.SchemaID)
	if err != nil {
		return 0, err
	}

	ptInfo := tblInfo.GetPartitionInfo()
	if ptInfo.GetNameByID(partitionID) == "" {
		job.State = model.JobStateCancelled
		return 0, errors.Trace(table.ErrUnknownPartition.GenWithStackByArgs("drop?", tblInfo.Name.O))
	}

	if len(rule.Labels) == 0 {
		patch := label.NewRulePatch([]*label.Rule{}, []string{rule.ID})
		err = infosync.UpdateLabelRules(context.TODO(), patch)
	} else {
		err = infosync.PutLabelRule(context.TODO(), rule)
	}
	if err != nil {
		job.State = model.JobStateCancelled
		return 0, errors.Wrapf(err, "failed to notify PD region label")
	}
	ver, err = updateVersionAndTableInfo(t, job, tblInfo, true)
	if err != nil {
		return ver, errors.Trace(err)
	}
	job.FinishTableJob(model.JobStateDone, model.StatePublic, ver, tblInfo)

	return ver, nil
}

func onAlterTablePartitionOptions(t *meta.Meta, job *model.Job) (ver int64, err error) {
	var partitionID int64
	policyRefInfo := &model.PolicyRefInfo{}
	placementSettings := &model.PlacementSettings{}
	err = job.DecodeArgs(&partitionID, policyRefInfo, placementSettings)
	if err != nil {
		job.State = model.JobStateCancelled
		return 0, errors.Trace(err)
	}
	tblInfo, err := getTableInfoAndCancelFaultJob(t, job, job.SchemaID)
	if err != nil {
		return 0, err
	}

	ptInfo := tblInfo.GetPartitionInfo()
	isFound := false
	definitions := ptInfo.Definitions
	for i := range definitions {
		if partitionID == definitions[i].ID {
			definitions[i].DirectPlacementOpts = placementSettings
			definitions[i].PlacementPolicyRef = policyRefInfo
			isFound = true
			break
		}
	}
	if !isFound {
		job.State = model.JobStateCancelled
		return 0, errors.Trace(table.ErrUnknownPartition.GenWithStackByArgs("drop?", tblInfo.Name.O))
	}

	ver, err = updateVersionAndTableInfo(t, job, tblInfo, true)
	if err != nil {
		return ver, errors.Trace(err)
	}
	job.FinishTableJob(model.JobStateDone, model.StatePublic, ver, tblInfo)

	return ver, nil
}

func getOldLabelRules(tblInfo *model.TableInfo, oldSchemaName, oldTableName string) (string, []string, []string, map[string]*label.Rule, error) {
	tableRuleID := fmt.Sprintf(label.TableIDFormat, label.IDPrefix, oldSchemaName, oldTableName)
	oldRuleIDs := []string{tableRuleID}
	var partRuleIDs []string
	if tblInfo.GetPartitionInfo() != nil {
		for _, def := range tblInfo.GetPartitionInfo().Definitions {
			partRuleIDs = append(partRuleIDs, fmt.Sprintf(label.PartitionIDFormat, label.IDPrefix, oldSchemaName, oldTableName, def.Name.L))
		}
	}

	oldRuleIDs = append(oldRuleIDs, partRuleIDs...)
	oldRules, err := infosync.GetLabelRules(context.TODO(), oldRuleIDs)
	return tableRuleID, partRuleIDs, oldRuleIDs, oldRules, err
}

func updateLabelRules(job *model.Job, tblInfo *model.TableInfo, oldRules map[string]*label.Rule, tableRuleID string, partRuleIDs, oldRuleIDs []string, tID int64) error {
	if oldRules == nil {
		return nil
	}
	var newRules []*label.Rule
	if tblInfo.GetPartitionInfo() != nil {
		for idx, def := range tblInfo.GetPartitionInfo().Definitions {
			if r, ok := oldRules[partRuleIDs[idx]]; ok {
				newRules = append(newRules, r.Clone().Reset(job.SchemaName, tblInfo.Name.L, def.Name.L, def.ID))
			}
		}
	}
	ids := []int64{tID}
	if r, ok := oldRules[tableRuleID]; ok {
		if tblInfo.GetPartitionInfo() != nil {
			for _, def := range tblInfo.GetPartitionInfo().Definitions {
				ids = append(ids, def.ID)
			}
		}
		newRules = append(newRules, r.Clone().Reset(job.SchemaName, tblInfo.Name.L, "", ids...))
	}

	patch := label.NewRulePatch(newRules, oldRuleIDs)
	return infosync.UpdateLabelRules(context.TODO(), patch)
}
