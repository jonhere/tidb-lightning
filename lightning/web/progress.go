package web

import (
	"encoding/json"
	"sync"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb-lightning/lightning/checkpoints"
	"github.com/pingcap/tidb-lightning/lightning/common"
	"github.com/pingcap/tidb-lightning/lightning/mydump"
)

// checkpointsMap is a concurrent map (table name → checkpoints).
//
// Implementation note: Currently the checkpointsMap is only written from a
// single goroutine inside (*RestoreController).listenCheckpointUpdates(), so
// all writes are going to be single threaded. Writing to checkpoint is not
// considered performance critical. The map can be read from any HTTP connection
// goroutine. Therefore, we simply implement the concurrent map using a single
// RWMutex. We may switch to more complicated data structure if contention is
// shown to be a problem.
//
// Do not implement this using a sync.Map, its mutex can't protect the content
// of a pointer.
type checkpointsMap struct {
	mu          sync.RWMutex
	checkpoints map[string]*checkpoints.TableCheckpoint
}

func makeCheckpointsMap() (res checkpointsMap) {
	res.checkpoints = make(map[string]*checkpoints.TableCheckpoint)
	return
}

func (cpm *checkpointsMap) clear() {
	cpm.mu.Lock()
	cpm.checkpoints = make(map[string]*checkpoints.TableCheckpoint)
	cpm.mu.Unlock()
}

func (cpm *checkpointsMap) insert(key string, cp *checkpoints.TableCheckpoint) {
	cpm.mu.Lock()
	cpm.checkpoints[key] = cp
	cpm.mu.Unlock()
}

type totalWritten struct {
	key          string
	totalWritten int64
}

func (cpm *checkpointsMap) update(diffs map[string]*checkpoints.TableCheckpointDiff) []totalWritten {
	totalWrittens := make([]totalWritten, 0, len(diffs))

	cpm.mu.Lock()
	defer cpm.mu.Unlock()

	for key, diff := range diffs {
		cp := cpm.checkpoints[key]
		cp.Apply(diff)

		tw := int64(0)
		for _, engine := range cp.Engines {
			for _, chunk := range engine.Chunks {
				if engine.Status >= checkpoints.CheckpointStatusAllWritten {
					tw += chunk.Chunk.EndOffset - chunk.Key.Offset
				} else {
					tw += chunk.Chunk.Offset - chunk.Key.Offset
				}
			}
		}
		totalWrittens = append(totalWrittens, totalWritten{key: key, totalWritten: tw})
	}
	return totalWrittens
}

func (cpm *checkpointsMap) marshal(key string) ([]byte, error) {
	cpm.mu.RLock()
	defer cpm.mu.RUnlock()

	if cp, ok := cpm.checkpoints[key]; ok {
		return json.Marshal(cp)
	}
	return nil, errors.NotFoundf("table %s", key)
}

type taskStatus uint8

const (
	taskStatusNotStarted taskStatus = 0
	taskStatusRunning               = 1
	taskStatusCompleted             = 2
)

type tableInfo struct {
	TotalWritten int64      `json:"w"`
	TotalSize    int64      `json:"z"`
	Status       taskStatus `json:"s"`
	Message      string     `json:"m,omitempty"`
}

type taskProgress struct {
	mu      sync.RWMutex
	Tables  map[string]*tableInfo `json:"t"`
	Status  taskStatus            `json:"s"`
	Message string                `json:"m,omitempty"`

	// The contents have their own mutex for protection
	checkpoints checkpointsMap
}

var currentProgress = taskProgress{
	checkpoints: makeCheckpointsMap(),
}

func BroadcastStartTask() {
	currentProgress.mu.Lock()
	currentProgress.Status = taskStatusRunning
	currentProgress.mu.Unlock()

	currentProgress.checkpoints.clear()
}

func BroadcastEndTask(err error) {
	errString := errors.ErrorStack(err)

	currentProgress.mu.Lock()
	currentProgress.Status = taskStatusCompleted
	currentProgress.Message = errString
	currentProgress.mu.Unlock()
}

func BroadcastInitProgress(databases []*mydump.MDDatabaseMeta) {
	tables := make(map[string]*tableInfo, len(databases))

	for _, db := range databases {
		for _, tbl := range db.Tables {
			name := common.UniqueTable(db.Name, tbl.Name)
			tables[name] = &tableInfo{TotalSize: tbl.TotalSize}
		}
	}

	currentProgress.mu.Lock()
	currentProgress.Tables = tables
	currentProgress.mu.Unlock()
}

func BroadcastTableCheckpoint(tableName string, cp *checkpoints.TableCheckpoint) {
	currentProgress.mu.Lock()
	currentProgress.Tables[tableName].Status = taskStatusRunning
	currentProgress.mu.Unlock()

	// create a deep copy to avoid false sharing
	currentProgress.checkpoints.insert(tableName, cp.DeepCopy())
}

func BroadcastCheckpointDiff(diffs map[string]*checkpoints.TableCheckpointDiff) {
	totalWrittens := currentProgress.checkpoints.update(diffs)

	currentProgress.mu.Lock()
	for _, tw := range totalWrittens {
		currentProgress.Tables[tw.key].TotalWritten = tw.totalWritten
	}
	currentProgress.mu.Unlock()
}

func BroadcastError(tableName string, err error) {
	errString := errors.ErrorStack(err)

	currentProgress.mu.Lock()
	if tbl := currentProgress.Tables[tableName]; tbl != nil {
		tbl.Status = taskStatusCompleted
		tbl.Message = errString
	}
	currentProgress.mu.Unlock()
}

func MarshalTaskProgress() ([]byte, error) {
	currentProgress.mu.RLock()
	defer currentProgress.mu.RUnlock()
	return json.Marshal(&currentProgress)
}

func MarshalTableCheckpoints(tableName string) ([]byte, error) {
	return currentProgress.checkpoints.marshal(tableName)
}
