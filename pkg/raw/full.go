package raw

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/google/btree"
	"github.com/pingcap/errors"
	"github.com/pingcap/kvproto/pkg/backup"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/log"
	"github.com/pingcap/parser/model"
	pd "github.com/pingcap/pd/client"
	"github.com/pingcap/tidb/ddl"
	"github.com/pingcap/tidb/domain"
	"github.com/pingcap/tidb/meta/autoid"
	"github.com/pingcap/tidb/session"
	"github.com/pingcap/tidb/store/tikv"
	"github.com/pingcap/tidb/tablecodec"
	"github.com/pingcap/tidb/util/codec"
	"github.com/pingcap/tidb/util/sqlexec"
	"go.uber.org/zap"

	"github.com/pingcap/br/pkg/meta"
	"github.com/pingcap/br/pkg/utils"
)

// Maximum total sleep time(in ms) for kv/cop commands.
const (
	backupFineGrainedMaxBackoff = 80000
)

// BackupClient is a client instructs TiKV how to do a backup.
type BackupClient struct {
	ctx    context.Context
	cancel context.CancelFunc

	backer    *meta.Backer
	clusterID uint64
	pdClient  pd.Client
	dom       *domain.Domain

	backupMeta    backup.BackupMeta
	backupSchemas backupSchemas
	storage       utils.ExternalStorage
}

// NewBackupClient returns a new backup client
func NewBackupClient(backer *meta.Backer) (*BackupClient, error) {
	log.Info("new backup client")
	ctx, cancel := context.WithCancel(backer.Context())
	pdClient := backer.GetPDClient()
	stores, err := pdClient.GetAllStores(ctx)
	if err != nil {
		cancel()
		return nil, errors.Trace(err)
	}
	// Do not run ddl worker in BR.
	ddl.RunWorker = false
	// Do not run stat worker in BR.
	session.DisableStats4Test()
	dom, err := session.BootstrapSession(backer.GetTiKV())
	if err != nil {
		cancel()
		return nil, errors.Trace(err)
	}
	poolSize := uint(len(stores) * 8)
	if poolSize > 100 {
		poolSize = 100
	}
	return &BackupClient{
		clusterID: pdClient.GetClusterID(ctx),
		backer:    backer,
		ctx:       ctx,
		cancel:    cancel,
		pdClient:  backer.GetPDClient(),
		dom:       dom,
		backupSchemas: backupSchemas{
			meta:       make(map[string]*backup.Schema),
			checksumCh: make(chan *tableChecksum),
			errCh:      make(chan error),
			wg:         sync.WaitGroup{},
			workerPool: utils.NewWorkerPool(poolSize, "restore"),
		},
	}, nil
}

// Close a backup client
func (bc *BackupClient) Close() {
	bc.dom.Close()
	bc.cancel()
}

// GetTS returns the latest timestamp.
func (bc *BackupClient) GetTS(timeAgo string) (uint64, error) {
	p, l, err := bc.pdClient.GetTS(bc.ctx)
	if err != nil {
		return 0, errors.Trace(err)
	}

	if timeAgo != "" {
		duration, err := time.ParseDuration(timeAgo)
		if err != nil {
			return 0, errors.Trace(err)
		}
		t := duration.Nanoseconds() / int64(time.Millisecond)
		log.Info("backup time ago", zap.Int64("MillisecondsAgo", t))

		// check backup time do not exceed GCSafePoint
		safePoint, err := bc.backer.GetGCSafePoint(bc.ctx)
		if err != nil {
			return 0, errors.Trace(err)
		}
		if p-t < safePoint.Physical {
			return 0, errors.New("given backup time exceed GCSafePoint")
		}
		p -= t
	}

	ts := meta.Timestamp{
		Physical: p,
		Logical:  l,
	}
	backupTS := meta.EncodeTs(ts)
	log.Info("backup encode timestamp", zap.Uint64("BackupTS", backupTS))
	return backupTS, nil
}

// SetStorage set ExternalStorage for client
func (bc *BackupClient) SetStorage(base string) error {
	var err error
	bc.storage, err = utils.CreateStorage(base)
	if err != nil {
		return err
	}
	// backupmeta already exists
	if exist := bc.storage.FileExists(utils.MetaFile); exist {
		return errors.New("backup meta exists, may be some backup files in the path already")
	}
	return nil
}

// SaveBackupMeta saves the current backup meta at the given path.
func (bc *BackupClient) SaveBackupMeta(path string) error {
	bc.backupMeta.Path = path
	backupMetaData, err := proto.Marshal(&bc.backupMeta)
	if err != nil {
		return errors.Trace(err)
	}
	log.Debug("backup meta",
		zap.Reflect("meta", bc.backupMeta))
	log.Info("save backup meta", zap.String("path", path))
	return bc.storage.Write(utils.MetaFile, backupMetaData)
}

// PreBackupTableRanges gets the range of table and request admin checksum from TiDB.
func (bc *BackupClient) PreBackupTableRanges(
	dbName, tableName string,
	path string,
	backupTS uint64,
) ([]Range, error) {
	dbSession, err := session.CreateSession(bc.backer.GetTiKV())
	if err != nil {
		return nil, errors.Trace(err)
	}
	info, err := bc.dom.GetSnapshotInfoSchema(backupTS)
	if err != nil {
		return nil, errors.Trace(err)
	}

	var dbInfo *model.DBInfo
	var tableInfo *model.TableInfo
	cDBName := model.NewCIStr(dbName)
	dbInfo, exist := info.SchemaByName(cDBName)
	if !exist {
		return nil, errors.Errorf("schema %s not found", dbName)
	}
	cTableName := model.NewCIStr(tableName)
	table, err := info.TableByName(cDBName, cTableName)
	if err != nil {
		return nil, errors.Trace(err)
	}
	tableInfo = table.Meta()
	idAlloc := autoid.NewAllocator(bc.backer.GetTiKV(), dbInfo.ID, false)
	globalAutoID, err := idAlloc.NextGlobalAutoID(tableInfo.ID)
	if err != nil {
		return nil, errors.Trace(err)
	}
	tableInfo.AutoIncID = globalAutoID

	dbData, err := json.Marshal(dbInfo)
	if err != nil {
		return nil, errors.Trace(err)
	}

	tableData, err := json.Marshal(tableInfo)
	if err != nil {
		return nil, errors.Trace(err)
	}

	backupSchema := &backup.Schema{
		Db:    dbData,
		Table: tableData,
	}
	// TODO figure out why
	// must set to true to avoid load global vars, otherwise we got error
	dbSession.GetSessionVars().CommonGlobalLoaded = true
	dbSession.GetSessionVars().SnapshotTS = backupTS
	bc.backupSchemas.startTableChecksum(bc.ctx, dbSession, backupSchema, dbInfo.Name.L, tableInfo.Name.L)

	log.Info("save table schema",
		zap.Stringer("db", dbInfo.Name),
		zap.Stringer("table", tableInfo.Name),
		zap.Int64("auto_inc_id", globalAutoID),
	)
	log.Info("backup table meta",
		zap.Reflect("Schema", dbInfo),
		zap.Reflect("Table", tableInfo))

	// TODO: We may need to include [t<tableID>, t<tableID+1>) in order to
	//       backup global index.
	tableRanges := buildTableRanges(tableInfo)
	ranges := make([]Range, 0, len(tableRanges))
	for _, r := range tableRanges {
		ranges = append(ranges, r.Range())
	}
	return ranges, nil
}

type tableRange struct {
	startID, endID int64
}

func (tr tableRange) Range() Range {
	startKey := tablecodec.GenTablePrefix(tr.startID)
	endKey := tablecodec.GenTablePrefix(tr.endID)
	return Range{
		StartKey: []byte(startKey),
		EndKey:   []byte(endKey),
	}
}

func buildTableRanges(tbl *model.TableInfo) []tableRange {
	pis := tbl.GetPartitionInfo()
	if pis == nil {
		// Short path, no partition.
		tableID := tbl.ID
		return []tableRange{{startID: tableID, endID: tableID + 1}}
	}

	ranges := make([]tableRange, 0, len(pis.Definitions))
	for _, def := range pis.Definitions {
		ranges = append(ranges,
			tableRange{
				startID: def.ID,
				endID:   def.ID + 1,
			})
	}
	return ranges
}

// PreBackupAllTableRanges gets the range of all tables and request admin checksum from TiDB.
func (bc *BackupClient) PreBackupAllTableRanges(backupTS uint64) ([]Range, error) {
	SystemDatabases := [3]string{
		"information_schema",
		"performance_schema",
		"mysql",
	}

	info, err := bc.dom.GetSnapshotInfoSchema(backupTS)
	if err != nil {
		return nil, errors.Trace(err)
	}

	dbInfos := info.AllSchemas()
	ranges := make([]Range, 0)
LoadDb:
	for _, dbInfo := range dbInfos {
		// skip system databases
		for _, sysDbName := range SystemDatabases {
			if sysDbName == dbInfo.Name.L {
				continue LoadDb
			}
		}
		dbData, err := json.Marshal(dbInfo)
		if err != nil {
			return nil, errors.Trace(err)
		}
		idAlloc := autoid.NewAllocator(bc.backer.GetTiKV(), dbInfo.ID, false)
		for _, tableInfo := range dbInfo.Tables {
			globalAutoID, err := idAlloc.NextGlobalAutoID(tableInfo.ID)
			if err != nil {
				return nil, errors.Trace(err)
			}
			tableInfo.AutoIncID = globalAutoID
			tableData, err := json.Marshal(tableInfo)
			if err != nil {
				return nil, errors.Trace(err)
			}

			backupSchema := &backup.Schema{
				Db:    dbData,
				Table: tableData,
			}
			dbSession, err := session.CreateSession(bc.backer.GetTiKV())
			if err != nil {
				return nil, errors.Trace(err)
			}
			dbSession.GetSessionVars().CommonGlobalLoaded = true
			// make FastChecksum snapshot is same as backup snapshot
			dbSession.GetSessionVars().SnapshotTS = backupTS
			bc.backupSchemas.startTableChecksum(bc.ctx, dbSession, backupSchema, dbInfo.Name.L, tableInfo.Name.L)

			// TODO: We may need to include [t<tableID>, t<tableID+1>) in order to
			//       backup global index.
			tableRanges := buildTableRanges(tableInfo)
			for _, r := range tableRanges {
				ranges = append(ranges, r.Range())
			}
		}
	}
	return ranges, nil
}

// BackupRanges make a backup of the given key ranges.
func (bc *BackupClient) BackupRanges(
	ranges []Range,
	path string,
	backupTS uint64,
	rate uint64,
	concurrency uint32,
	updateCh chan<- struct{},
) error {
	start := time.Now()
	defer func() {
		elapsed := time.Since(start)
		log.Info("Backup Ranges", zap.Duration("take", elapsed))
	}()

	errCh := make(chan error)
	ctx, cancel := context.WithCancel(bc.ctx)
	defer cancel()
	go func() {
		for _, r := range ranges {
			err := bc.backupRange(
				ctx, r.StartKey, r.EndKey, path, backupTS, rate, concurrency, updateCh)
			if err != nil {
				errCh <- err
				return
			}
		}
		close(errCh)
	}()

	// Check GC safepoint every 30s.
	t := time.NewTicker(time.Second * 30)
	defer t.Stop()

	finished := false
	for {
		err := bc.backer.CheckGCSafepoint(ctx, backupTS)
		if err != nil {
			// Ignore the error since it retries every 30s.
			log.Warn("get GC safepoint failed", zap.Error(err))
		}
		if finished {
			// Return error (if there is any) before finishing backup.
			return err
		}
		select {
		case err, ok := <-errCh:
			if !ok {
				// Before finish backup, we have to make sure
				// the backup ts does not fall behind with GC safepoint.
				finished = true
			}
			if err != nil {
				return err
			}
		case <-t.C:
		}
	}
}

// backupRange make a backup of the given key range.
func (bc *BackupClient) backupRange(
	ctx context.Context,
	startKey, endKey []byte,
	path string,
	backupTS uint64,
	rateMBs uint64,
	concurrency uint32,
	updateCh chan<- struct{},
) error {
	// The unit of rate limit in protocol is bytes per second.
	rateLimit := rateMBs * 1024 * 1024
	log.Info("backup started",
		zap.Binary("StartKey", startKey),
		zap.Binary("EndKey", endKey),
		zap.Uint64("RateLimit", rateMBs),
		zap.Uint32("Concurrency", concurrency))
	start := time.Now()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	allStores, err := bc.pdClient.GetAllStores(ctx)
	if err != nil {
		return errors.Trace(err)
	}
	req := backup.BackupRequest{
		ClusterId:    bc.clusterID,
		StartKey:     startKey,
		EndKey:       endKey,
		StartVersion: backupTS,
		EndVersion:   backupTS,
		Path:         path,
		RateLimit:    rateLimit,
		Concurrency:  concurrency,
	}
	push := newPushDown(ctx, bc.backer, len(allStores))

	results, err := push.pushBackup(req, allStores, updateCh)
	if err != nil {
		return err
	}
	log.Info("finish backup push down", zap.Int("Ok", results.len()))

	// Find and backup remaining ranges.
	// TODO: test fine grained backup.
	err = bc.fineGrainedBackup(
		startKey, endKey,
		backupTS, path, rateLimit, concurrency, results, updateCh)
	if err != nil {
		return err
	}

	bc.backupMeta.StartVersion = backupTS
	bc.backupMeta.EndVersion = backupTS
	log.Info("backup time range",
		zap.Reflect("StartVersion", backupTS),
		zap.Reflect("EndVersion", backupTS))

	results.tree.Ascend(func(i btree.Item) bool {
		r := i.(*Range)
		bc.backupMeta.Files = append(bc.backupMeta.Files, r.Files...)
		return true
	})

	// Check if there are duplicated files.
	results.checkDupFiles()

	log.Info("backup range finished",
		zap.Duration("take", time.Since(start)))
	return nil
}

func (bc *BackupClient) findRegionLeader(key []byte) (*metapb.Peer, error) {
	// Keys are saved in encoded format in TiKV, so the key must be encoded
	// in order to find the correct region.
	key = codec.EncodeBytes([]byte{}, key)
	for i := 0; i < 5; i++ {
		// better backoff.
		_, leader, err := bc.pdClient.GetRegion(bc.ctx, key)
		if err != nil {
			log.Error("find region failed", zap.Error(err))
			time.Sleep(time.Millisecond * time.Duration(100*i))
			continue
		}
		if leader != nil {
			log.Info("find region",
				zap.Reflect("Leader", leader), zap.Binary("Key", key))
			return leader, nil
		}
		log.Warn("no region found", zap.Binary("Key", key))
		time.Sleep(time.Millisecond * time.Duration(100*i))
		continue
	}
	return nil, errors.Errorf("can not find region for key %v", key)
}

func (bc *BackupClient) fineGrainedBackup(
	startKey, endKey []byte,
	backupTS uint64,
	path string,
	rateLimit uint64,
	concurrency uint32,
	rangeTree RangeTree,
	updateCh chan<- struct{},
) error {
	bo := tikv.NewBackoffer(bc.ctx, backupFineGrainedMaxBackoff)
	for {
		// Step1, check whether there is any incomplete range
		incomplete := rangeTree.getIncompleteRange(startKey, endKey)
		if len(incomplete) == 0 {
			return nil
		}
		log.Info("start fine grained backup", zap.Int("incomplete", len(incomplete)))
		// Step2, retry backup on incomplete range
		respCh := make(chan *backup.BackupResponse, 4)
		errCh := make(chan error, 4)
		retry := make(chan Range, 4)

		max := &struct {
			ms int
			mu sync.Mutex
		}{}
		wg := sync.WaitGroup{}
		for i := 0; i < 4; i++ {
			wg.Add(1)
			fork, _ := bo.Fork()
			go func(boFork *tikv.Backoffer) {
				defer wg.Done()
				for rg := range retry {
					backoffMs, err :=
						bc.handleFineGrained(boFork, rg, backupTS, path, rateLimit, concurrency, respCh)
					if err != nil {
						errCh <- err
						return
					}
					if backoffMs != 0 {
						max.mu.Lock()
						if max.ms < backoffMs {
							max.ms = backoffMs
						}
						max.mu.Unlock()
					}
				}
			}(fork)
		}

		// Dispatch rangs and wait
		go func() {
			for _, rg := range incomplete {
				retry <- rg
			}
			close(retry)
			wg.Wait()
			close(respCh)
		}()

	selectLoop:
		for {
			select {
			case err := <-errCh:
				// TODO: should we handle err here?
				return err
			case resp, ok := <-respCh:
				if !ok {
					// Finished.
					break selectLoop
				}
				if resp.Error != nil {
					log.Fatal("unexpected backup error",
						zap.Reflect("error", resp.Error))
				}
				log.Info("put fine grained range",
					zap.Binary("StartKey", resp.StartKey),
					zap.Binary("EndKey", resp.EndKey),
				)
				rangeTree.putOk(resp.StartKey, resp.EndKey, resp.Files)

				// Update progress
				updateCh <- struct{}{}
			}
		}

		// Step3. Backoff if needed, then repeat.
		max.mu.Lock()
		ms := max.ms
		max.mu.Unlock()
		if ms != 0 {
			log.Info("handle fine grained", zap.Int("backoffMs", ms))
			err := bo.BackoffWithMaxSleep(2, /* magic boTxnLockFast */
				ms, errors.New("TODO: attach error"))
			if err != nil {
				return errors.Trace(err)
			}
		}
	}
}

func onBackupResponse(
	bo *tikv.Backoffer,
	lockResolver *tikv.LockResolver,
	resp *backup.BackupResponse,
) (*backup.BackupResponse, int, error) {
	log.Debug("onBackupResponse", zap.Reflect("resp", resp))
	if resp.Error == nil {
		return resp, 0, nil
	}
	backoffMs := 0
	switch v := resp.Error.Detail.(type) {
	case *backup.Error_KvError:
		if lockErr := v.KvError.Locked; lockErr != nil {
			// Try to resolve lock.
			log.Warn("backup occur kv error", zap.Reflect("error", v))
			msBeforeExpired, err1 := lockResolver.ResolveLocks(
				bo, []*tikv.Lock{tikv.NewLock(lockErr)})
			if err1 != nil {
				return nil, 0, errors.Trace(err1)
			}
			if msBeforeExpired > 0 {
				backoffMs = int(msBeforeExpired)
			}
			return nil, backoffMs, nil
		}
		// Backup should not meet error other than KeyLocked.
		log.Error("unexpect kv error", zap.Reflect("KvError", v.KvError))
		return nil, backoffMs, errors.Errorf("onBackupResponse error %v", v)

	case *backup.Error_RegionError:
		regionErr := v.RegionError
		// Ignore following errors.
		if !(regionErr.EpochNotMatch != nil ||
			regionErr.NotLeader != nil ||
			regionErr.RegionNotFound != nil ||
			regionErr.ServerIsBusy != nil ||
			regionErr.StaleCommand != nil ||
			regionErr.StoreNotMatch != nil) {
			log.Error("unexpect region error",
				zap.Reflect("RegionError", regionErr))
			return nil, backoffMs, errors.Errorf("onBackupResponse error %v", v)
		}
		log.Warn("backup occur region error",
			zap.Reflect("RegionError", regionErr))
		// TODO: a better backoff.
		backoffMs = 1000 /* 1s */
		return nil, backoffMs, nil
	case *backup.Error_ClusterIdError:
		log.Error("backup occur cluster ID error",
			zap.Reflect("error", v))
		err := errors.Errorf("%v", resp.Error)
		return nil, 0, err
	default:
		log.Error("backup occur unknown error",
			zap.String("error", resp.Error.GetMsg()))
		err := errors.Errorf("%v", resp.Error)
		return nil, 0, err
	}
}

func (bc *BackupClient) handleFineGrained(
	bo *tikv.Backoffer,
	rg Range,
	backupTS uint64,
	path string,
	rateLimit uint64,
	concurrency uint32,
	respCh chan<- *backup.BackupResponse,
) (int, error) {
	leader, pderr := bc.findRegionLeader(rg.StartKey)
	if pderr != nil {
		return 0, pderr
	}
	max := 0
	req := backup.BackupRequest{
		ClusterId:    bc.clusterID,
		StartKey:     rg.StartKey, // TODO: the range may cross region.
		EndKey:       rg.EndKey,
		StartVersion: backupTS,
		EndVersion:   backupTS,
		Path:         path,
		RateLimit:    rateLimit,
		Concurrency:  concurrency,
	}
	lockResolver := bc.backer.GetLockResolver()
	err := bc.backer.SendBackup(
		bc.ctx, leader.GetStoreId(), req,
		// Handle responses with the same backoffer.
		func(resp *backup.BackupResponse) error {
			response, backoffMs, err :=
				onBackupResponse(bo, lockResolver, resp)
			if err != nil {
				return err
			}
			if max < backoffMs {
				max = backoffMs
			}
			if response != nil {
				respCh <- response
			}
			return nil
		})
	if err != nil {
		return 0, err
	}
	return max, nil
}

// GetRangeRegionCount get region count by pd http api
func (bc *BackupClient) GetRangeRegionCount(startKey, endKey []byte) (int, error) {
	return bc.backer.GetRegionCount()
}

// FastChecksum check data integrity by xor all(sst_checksum) per table
func (bc *BackupClient) FastChecksum() (bool, error) {
	start := time.Now()
	defer func() {
		elapsed := time.Since(start)
		log.Info("Backup Checksum", zap.Duration("take", elapsed))
	}()

	dbs, err := utils.LoadBackupTables(&bc.backupMeta)
	if err != nil {
		return false, err
	}

	for _, schema := range bc.backupMeta.Schemas {
		dbInfo := &model.DBInfo{}
		err = json.Unmarshal(schema.Db, dbInfo)
		if err != nil {
			return false, err
		}
		tblInfo := &model.TableInfo{}
		err = json.Unmarshal(schema.Table, tblInfo)
		if err != nil {
			return false, err
		}
		tbl := dbs[dbInfo.Name.String()].GetTable(tblInfo.Name.String())

		checksum := uint64(0)
		totalKvs := uint64(0)
		totalBytes := uint64(0)
		for _, file := range tbl.Files {
			checksum ^= file.Crc64Xor
			totalKvs += file.TotalKvs
			totalBytes += file.TotalBytes
		}
		if schema.Crc64Xor == checksum && schema.TotalKvs == totalKvs && schema.TotalBytes == totalBytes {
			log.Info("fast checksum success", zap.Stringer("db", dbInfo.Name), zap.Stringer("table", tblInfo.Name))
		} else {
			log.Error("failed in fast checksum",
				zap.String("database", dbInfo.Name.String()),
				zap.String("table", tblInfo.Name.String()),
				zap.Uint64("origin tidb crc64", schema.Crc64Xor),
				zap.Uint64("calculated crc64", checksum),
				zap.Uint64("origin tidb total kvs", schema.TotalKvs),
				zap.Uint64("calculated total kvs", totalKvs),
				zap.Uint64("origin tidb total bytes", schema.TotalBytes),
				zap.Uint64("calculated total bytes", totalBytes),
			)
			return false, nil
		}
	}

	return true, nil
}

// CompleteMeta wait response of admin checksum from TiDB to complete backup meta
func (bc *BackupClient) CompleteMeta() error {
	schemas, err := bc.backupSchemas.finishTableChecksum()
	if err != nil {
		return err
	}
	bc.backupMeta.Schemas = schemas
	return nil
}

type tableChecksum struct {
	name       string
	checksum   uint64
	totalKvs   uint64
	totalBytes uint64
	duration   time.Duration
}

func getChecksumFromTiDB(
	ctx context.Context,
	dbSession session.Session,
	dbName string,
	tableName string,
) (*tableChecksum, error) {
	start := time.Now()
	var recordSets []sqlexec.RecordSet
	recordSets, err := dbSession.Execute(ctx, fmt.Sprintf(
		"ADMIN CHECKSUM TABLE %s.%s", utils.EncloseName(dbName), utils.EncloseName(tableName)))
	if err != nil {
		return nil, errors.Trace(err)
	}
	records, err := utils.ResultSetToStringSlice(ctx, dbSession, recordSets[0])
	if err != nil {
		return nil, errors.Trace(err)
	}

	record := records[0]
	checksum, err := strconv.ParseUint(record[2], 10, 64)
	if err != nil {
		return nil, errors.Trace(err)
	}
	totalKvs, err := strconv.ParseUint(record[3], 10, 64)
	if err != nil {
		return nil, errors.Trace(err)
	}
	totalBytes, err := strconv.ParseUint(record[4], 10, 64)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return &tableChecksum{
		checksum:   checksum,
		totalKvs:   totalKvs,
		totalBytes: totalBytes,
		duration:   time.Since(start),
	}, nil
}

type backupSchemas struct {
	meta       map[string]*backup.Schema
	checksumCh chan *tableChecksum
	errCh      chan error
	wg         sync.WaitGroup
	workerPool *utils.WorkerPool
}

func (bs *backupSchemas) startTableChecksum(
	ctx context.Context,
	dbSession session.Session,
	schema *backup.Schema,
	dbName, tableName string) {
	name := fmt.Sprintf("%s.%s", dbName, tableName)
	log.Info("admin checksum from TiDB start",
		zap.String("table", name),
	)
	bs.meta[name] = schema
	bs.wg.Add(1)
	// TODO: It may block backup if len(table) > len(workers).
	bs.workerPool.Apply(func() {
		defer bs.wg.Done()
		checksum, err := getChecksumFromTiDB(ctx, dbSession, dbName, tableName)
		if err != nil {
			bs.errCh <- err
			return
		}
		checksum.name = name
		bs.checksumCh <- checksum
	})
}

func (bs *backupSchemas) finishTableChecksum() ([]*backup.Schema, error) {
	go func() {
		bs.wg.Wait()
		close(bs.checksumCh)
	}()
	var schemas []*backup.Schema
	for {
		select {
		case checksum, ok := <-bs.checksumCh:
			if !ok {
				return schemas, nil
			}
			log.Info("admin checksum from TiDB finished",
				zap.String("table", checksum.name),
				zap.Uint64("Crc64Xor", checksum.checksum),
				zap.Uint64("TotalKvs", checksum.totalKvs),
				zap.Uint64("TotalBytes", checksum.totalBytes),
				zap.Duration("take", checksum.duration))
			s := bs.meta[checksum.name]
			s.Crc64Xor = checksum.checksum
			s.TotalKvs = checksum.totalKvs
			s.TotalBytes = checksum.totalBytes
			schemas = append(schemas, s)
		case err := <-bs.errCh:
			return nil, errors.Trace(err)
		}
	}
}
