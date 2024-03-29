package server

import (
	"context"
	"github.com/pingcap-incubator/tinykv/kv/util/engine_util"

	"github.com/pingcap-incubator/tinykv/kv/coprocessor"
	"github.com/pingcap-incubator/tinykv/kv/storage"
	"github.com/pingcap-incubator/tinykv/kv/storage/raft_storage"
	"github.com/pingcap-incubator/tinykv/kv/transaction/latches"
	"github.com/pingcap-incubator/tinykv/kv/transaction/mvcc"
	coppb "github.com/pingcap-incubator/tinykv/proto/pkg/coprocessor"
	"github.com/pingcap-incubator/tinykv/proto/pkg/kvrpcpb"
	"github.com/pingcap-incubator/tinykv/proto/pkg/tinykvpb"
	"github.com/pingcap/tidb/kv"
)

var _ tinykvpb.TinyKvServer = new(Server)

// Server is a TinyKV server, it 'faces outwards', sending and receiving messages from clients such as TinySQL.
type Server struct {
	storage storage.Storage

	// (Used in 4A/4B)
	// 理解是：在对一堆key上锁时，需要将锁放入底层存储，这需要一定时间，此时如果并发来了其他的key会出现一个锁被锁多次的情况
	// 因此每次要操作key时都需要对其先latch
	// 但又感觉用不到，因为都是要过raft的，实际查找是线性的
	Latches *latches.Latches

	// coprocessor API handler, out of course scope
	copHandler *coprocessor.CopHandler
}

func NewServer(storage storage.Storage) *Server {
	return &Server{
		storage: storage,
		Latches: latches.NewLatches(),
	}
}

// The below functions are Server's gRPC API (implements TinyKvServer).

// Raft commands (tinykv <-> tinykv)
// Only used for RaftStorage, so trivially forward it.
func (server *Server) Raft(stream tinykvpb.TinyKv_RaftServer) error {
	return server.storage.(*raft_storage.RaftStorage).Raft(stream)
}

// Snapshot stream (tinykv <-> tinykv)
// Only used for RaftStorage, so trivially forward it.
func (server *Server) Snapshot(stream tinykvpb.TinyKv_SnapshotServer) error {
	return server.storage.(*raft_storage.RaftStorage).Snapshot(stream)
}

// Transactional API.

func (server *Server) bindGetErr(resp *kvrpcpb.GetResponse, err error) (*kvrpcpb.GetResponse, error) {
	if regionErr, ok := err.(*raft_storage.RegionError); ok {
		resp.RegionError = regionErr.RequestErr
		return resp, nil
	}
	return nil, err
}
func (server *Server) bindPreWriteErr(resp *kvrpcpb.PrewriteResponse, err error) (*kvrpcpb.PrewriteResponse, error) {
	if regionErr, ok := err.(*raft_storage.RegionError); ok {
		resp.RegionError = regionErr.RequestErr
		return resp, nil
	}
	return nil, err
}
func (server *Server) bindCommitErr(resp *kvrpcpb.CommitResponse, err error) (*kvrpcpb.CommitResponse, error) {
	if regionErr, ok := err.(*raft_storage.RegionError); ok {
		resp.RegionError = regionErr.RequestErr
		return resp, nil
	}
	return nil, err
}
func (server *Server) bindScanErr(resp *kvrpcpb.ScanResponse, err error) (*kvrpcpb.ScanResponse, error) {
	if regionErr, ok := err.(*raft_storage.RegionError); ok {
		resp.RegionError = regionErr.RequestErr
		return resp, nil
	}
	return nil, err
}

func (server *Server) bindCheckTxnStatusErr(resp *kvrpcpb.CheckTxnStatusResponse, err error) (*kvrpcpb.CheckTxnStatusResponse, error) {
	if regionErr, ok := err.(*raft_storage.RegionError); ok {
		resp.RegionError = regionErr.RequestErr
		return resp, nil
	}
	return nil, err
}

func (server *Server) bindRollbackErr(resp *kvrpcpb.BatchRollbackResponse, err error) (*kvrpcpb.BatchRollbackResponse, error) {
	if regionErr, ok := err.(*raft_storage.RegionError); ok {
		resp.RegionError = regionErr.RequestErr
		return resp, nil
	}
	return nil, err
}
func (server *Server) bindResolveErr(resp *kvrpcpb.ResolveLockResponse, err error) (*kvrpcpb.ResolveLockResponse, error) {
	if regionErr, ok := err.(*raft_storage.RegionError); ok {
		resp.RegionError = regionErr.RequestErr
		return resp, nil
	}
	return nil, err
}
func (server *Server) KvGet(_ context.Context, req *kvrpcpb.GetRequest) (*kvrpcpb.GetResponse, error) {
	// Your Code Here (4B).
	resp := new(kvrpcpb.GetResponse)
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		return server.bindGetErr(resp, err)
	}
	defer reader.Close()

	txn := mvcc.NewMvccTxn(reader, req.Version)

	//在时间戳范围 [0,开始时间戳]（是右开区间） 内检查有没有锁
	// 这个范围是在此次事务快照所有可见的时间戳。如果看到一个锁，表示另一个事务在并发的写这个cell，所以读事务必须等待直到此锁释放
	lock, err1 := txn.GetLock(req.Key)
	if err1 != nil {
		return server.bindGetErr(resp, err1)
	}

	if lock != nil && lock.Ts < req.Version {
		resp.Error = &kvrpcpb.KeyError{
			Locked: lock.Info(req.Key),
		}
		return resp, nil
	}
	value, err2 := txn.GetValue(req.Key)
	if err2 != nil {
		return server.bindGetErr(resp, err2)
	}
	if value == nil {
		resp.NotFound = true
	} else {
		resp.Value = value
	}
	return resp, nil
}

func (server *Server) KvPrewrite(_ context.Context, req *kvrpcpb.PrewriteRequest) (*kvrpcpb.PrewriteResponse, error) {
	// Your Code Here (4B).
	resp := new(kvrpcpb.PrewriteResponse)
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		return server.bindPreWriteErr(resp, err)
	}
	defer reader.Close()

	txn := mvcc.NewMvccTxn(reader, req.StartVersion)

	// Most responses have a way to indicate non-fatal errors for situations like a key being locked. By reporting these to the client, it can retry a transaction after some time.
	// 记录下所有的KeyError
	keyErrors := make([]*kvrpcpb.KeyError, 0)

	for _, mutation := range req.Mutations {
		key := mutation.Key
		// 如果事务在它的开始时间戳之后看见另一个写记录，它会取消，这是“写-写”冲突
		recentWrite, recentCommitTS, err := txn.MostRecentWrite(key)
		if err != nil {
			return server.bindPreWriteErr(resp, err)
		}
		// 如果在 rollback 之后收到同一个事务的 prewrite，则会由于 prewrite 的这部分代码而直接返回错误
		if recentWrite != nil && recentCommitTS >= req.StartVersion {
			keyErrors = append(keyErrors, &kvrpcpb.KeyError{
				Conflict: &kvrpcpb.WriteConflict{
					StartTs:    req.StartVersion,
					ConflictTs: recentCommitTS,
					Key:        key,
					Primary:    req.PrimaryLock,
				},
			})
			continue
		}

		// 如果事务在任意时间戳看见另一个锁，它也取消
		lock, err1 := txn.GetLock(key)
		if err1 != nil {
			return server.bindPreWriteErr(resp, err1)
		}
		// 需要判断所属事务是否一致
		if lock != nil && lock.Ts != req.StartVersion {
			keyErrors = append(keyErrors, &kvrpcpb.KeyError{
				Locked: lock.Info(key),
			})
			continue
		}

		// lock写入
		lock = &mvcc.Lock{
			Primary: req.PrimaryLock,
			Ts:      req.StartVersion,
			Ttl:     req.LockTtl,
		}
		switch mutation.Op {
		case kvrpcpb.Op_Put:
			lock.Kind = mvcc.WriteKindPut
			// value写入
			txn.PutValue(key, mutation.Value)
		case kvrpcpb.Op_Del:
			lock.Kind = mvcc.WriteKindDelete
			txn.DeleteValue(key)
		case kvrpcpb.Op_Rollback:
			lock.Kind = mvcc.WriteKindRollback
		}
		txn.PutLock(key, lock)
	}
	if len(keyErrors) != 0 {
		resp.Errors = keyErrors
		return resp, nil
	}
	// 将所有操作写入底层存储
	err = server.storage.Write(req.Context, txn.Writes())
	if err != nil {
		return server.bindPreWriteErr(resp, err)
	}
	return resp, nil
}

func (server *Server) KvCommit(_ context.Context, req *kvrpcpb.CommitRequest) (*kvrpcpb.CommitResponse, error) {
	// Your Code Here (4B).
	resp := new(kvrpcpb.CommitResponse)
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		return server.bindCommitErr(resp, err)
	}
	defer reader.Close()

	txn := mvcc.NewMvccTxn(reader, req.StartVersion)

	for _, key := range req.Keys {
		// 首先检查是否存在行锁，如不存在则已经 commit 或 rollback
		lock, err := txn.GetLock(key)
		if err != nil {
			return server.bindCommitErr(resp, err)
		}

		if lock != nil && lock.Ts == req.StartVersion {
			// 正常情况下，该 key 应当存在同一个事务的锁,标记commit
			txn.PutWrite(key, req.CommitVersion, &mvcc.Write{
				StartTS: req.StartVersion,
				// 只有lock中能拿到kind，难道一个事务中对一个key只修改一次吗
				Kind: lock.Kind,
			})
			txn.DeleteLock(key)
			continue
		}

		// 找到 start_ts 与当前事务的 start_ts 相等的提交记录
		currentWrite, _, err1 := txn.CurrentWrite(key)
		if err1 != nil {
			return server.bindCommitErr(resp, err1)
		}

		// TestCommitMissingPrewrite4a测试，没有锁且没有commit，但现在发commit了，忽略它（不理解为什么有这测试）
		if lock == nil && currentWrite == nil {
			continue
		}
		// 测试有currentWrite为nil的情况，应该就是出现故障丢失了？
		if currentWrite == nil || currentWrite.Kind == mvcc.WriteKindRollback {
			//该事务被回滚。比如，如果由于网络原因迟迟不能成功提交，直到锁 TTL 超时时，事务便有可能被其它事务回滚。
			resp.Error = &kvrpcpb.KeyError{
				Retryable: "true",
			}
		} else if currentWrite.Kind != mvcc.WriteKindRollback {
			// (详见TiKV源码解析)该 key 已经成功提交。比如，当网络原因导致客户端没能收到提交成功的响应、因而发起重试时，可能会发生这种情况。
			// 此外，锁可能被另一个遇到锁的事务抢先提交（见下文“处理残留的锁”一节），这样的话也会发生这种情况。在这种情况下，不进行任何操作返回成功（为了幂等）
			//resp.Error = &kvrpcpb.KeyError{
			//	Retryable:            "false",
			//}
		}
		return resp, nil
	}

	// 将所有操作写入底层存储
	err = server.storage.Write(req.Context, txn.Writes())
	if err != nil {
		return server.bindCommitErr(resp, err)
	}
	return resp, nil
}

func (server *Server) KvScan(_ context.Context, req *kvrpcpb.ScanRequest) (*kvrpcpb.ScanResponse, error) {
	// Your Code Here (4C).
	resp := new(kvrpcpb.ScanResponse)
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		return server.bindScanErr(resp, err)
	}
	defer reader.Close()

	txn := mvcc.NewMvccTxn(reader, req.Version)
	// 扫描全表获取不同的key
	scan := mvcc.NewScanner(req.StartKey, txn)
	defer scan.Close()
	cnt := req.GetLimit()
	for cnt != 0 {
		key, value, err := scan.Next()
		if err != nil {
			return server.bindScanErr(resp, err)
		}

		//查找完成，没有值了
		if key == nil && value == nil {
			break
		}

		// 被delete了，应该找下一个key
		if value == nil {
			continue
		}

		// 在时间戳范围 [0,开始时间戳]（是右开区间） 内检查有没有锁
		// 这个范围是在此次事务快照所有可见的时间戳。如果看到一个锁，表示另一个事务在并发的写这个cell，所以读事务必须等待直到此锁释放
		lock, err1 := txn.GetLock(key)
		if err1 != nil {
			return server.bindScanErr(resp, err1)
		}

		cnt--

		// 有锁了
		if lock != nil && lock.Ts < req.Version {
			resp.Pairs = append(resp.Pairs, &kvrpcpb.KvPair{
				Error: &kvrpcpb.KeyError{
					Locked: lock.Info(key),
				},
				Key:   nil,
				Value: nil,
			})
			continue
		}

		resp.Pairs = append(resp.Pairs, &kvrpcpb.KvPair{
			Error: nil,
			Key:   key,
			Value: value,
		})
	}

	return resp, nil
}

func (server *Server) KvCheckTxnStatus(_ context.Context, req *kvrpcpb.CheckTxnStatusRequest) (*kvrpcpb.CheckTxnStatusResponse, error) {
	// Your Code Here (4C).
	resp := new(kvrpcpb.CheckTxnStatusResponse)
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		return server.bindCheckTxnStatusErr(resp, err)
	}
	defer reader.Close()

	txn := mvcc.NewMvccTxn(reader, req.LockTs)
	lock, err1 := txn.GetLock(req.PrimaryKey)
	if err1 != nil {
		return server.bindCheckTxnStatusErr(resp, err1)
	}
	if lock != nil && lock.Ts == req.LockTs {
		// 超时，使用物理时钟判定
		if mvcc.PhysicalTime(req.CurrentTs) >= mvcc.PhysicalTime(req.LockTs)+lock.Ttl {
			// If the TTL of the transaction is exhausted, abort that transaction and roll back the primary lock.
			// 删锁并rollback
			txn.DeleteLock(req.PrimaryKey)
			// rollback的commitTS设置为startTS
			txn.PutWrite(req.PrimaryKey, req.LockTs, &mvcc.Write{
				StartTS: req.LockTs,
				Kind:    mvcc.WriteKindRollback,
			})
			txn.DeleteValue(req.PrimaryKey)
			err = server.storage.Write(req.Context, txn.Writes())
			if err != nil {
				return server.bindCheckTxnStatusErr(resp, err)
			}
			// The lock is rolled back because it has expired.
			resp.Action = kvrpcpb.Action_TTLExpireRollback
		} else {
			// Otherwise, returns the TTL information.
			// locked: lock_ttl > 0
			resp.LockTtl = lock.Ttl
		}
	} else {
		// If the transaction has previously been rolled back or committed, return that information.
		write, commitTS, err := txn.CurrentWrite(req.PrimaryKey)
		if err != nil {
			return server.bindCheckTxnStatusErr(resp, err)
		}
		if write == nil {
			// The lock does not exist, TinyKV left a record of the rollback, but did not
			// have to delete a lock.
			// 没有匹配的锁，也没有write，需要留一个Rollback
			txn.PutWrite(req.PrimaryKey, req.LockTs, &mvcc.Write{
				StartTS: req.LockTs,
				Kind:    mvcc.WriteKindRollback,
			})
			err = server.storage.Write(req.Context, txn.Writes())
			if err != nil {
				return server.bindCheckTxnStatusErr(resp, err)
			}
			resp.Action = kvrpcpb.Action_LockNotExistRollback
		} else if write.Kind != mvcc.WriteKindRollback {
			// committed: commit_version > 0
			resp.CommitVersion = commitTS
		} else {
			// 已经RollBack了,测试需要NoAction
			resp.Action = kvrpcpb.Action_NoAction
		}
	}
	return resp, nil
}

func (server *Server) KvBatchRollback(_ context.Context, req *kvrpcpb.BatchRollbackRequest) (*kvrpcpb.BatchRollbackResponse, error) {
	// Your Code Here (4C).
	resp := new(kvrpcpb.BatchRollbackResponse)
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		return server.bindRollbackErr(resp, err)
	}
	defer reader.Close()

	txn := mvcc.NewMvccTxn(reader, req.StartVersion)
	for _, key := range req.Keys {
		lock, err1 := txn.GetLock(key)
		if err1 != nil {
			return server.bindRollbackErr(resp, err1)
		}

		// 存在当前事务的行锁,rollback
		if lock != nil && lock.Ts == req.StartVersion {
			txn.DeleteLock(key)
			txn.DeleteValue(key)
			txn.PutWrite(key, req.StartVersion, &mvcc.Write{
				StartTS: req.StartVersion,
				Kind:    mvcc.WriteKindRollback,
			})
			continue
		}

		write, _, err := txn.CurrentWrite(key)
		if err != nil {
			server.bindRollbackErr(resp, err)
		}

		if write == nil {
			// 不存在write需要写入rollback
			txn.PutWrite(key, req.StartVersion, &mvcc.Write{
				StartTS: req.StartVersion,
				Kind:    mvcc.WriteKindRollback,
			})
			continue
		}

		// Will fail if the transaction has already been committed or keys are locked by a different transaction.
		if write.Kind != mvcc.WriteKindRollback || lock != nil {
			resp.Error = &kvrpcpb.KeyError{
				Abort: "true",
			}
			return resp, nil
		}
		//If the keys were never locked, no action is needed but it is not an error.
	}

	// 将所有操作写入底层存储
	err = server.storage.Write(req.Context, txn.Writes())
	if err != nil {
		return server.bindRollbackErr(resp, err)
	}
	return resp, nil
}

func (server *Server) KvResolveLock(_ context.Context, req *kvrpcpb.ResolveLockRequest) (*kvrpcpb.ResolveLockResponse, error) {
	// Your Code Here (4C).
	resp := new(kvrpcpb.ResolveLockResponse)
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		return server.bindResolveErr(resp, err)
	}
	defer reader.Close()

	txn := mvcc.NewMvccTxn(reader, req.StartVersion)

	iter := reader.IterCF(engine_util.CfLock)
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		item := iter.Item()
		lockByte, err := item.Value()
		if err != nil {
			return server.bindResolveErr(resp, err)
		}
		if lockByte == nil {
			continue
		}
		lock, err1 := mvcc.ParseLock(lockByte)
		if err1 != nil {
			return server.bindResolveErr(resp, err1)
		}
		// 找到所有该transaction的lock
		if lock.Ts != req.StartVersion {
			continue
		}

		key := item.Key()
		if req.CommitVersion == 0 {
			// If commit_version is 0, TinyKV will rollback all locks.
			txn.DeleteLock(key)
			txn.DeleteValue(key)
			txn.PutWrite(key, req.StartVersion, &mvcc.Write{
				StartTS: req.StartVersion,
				Kind:    mvcc.WriteKindRollback,
			})
		} else {
			// commit those locks with the given commit timestamp
			txn.PutWrite(key, req.CommitVersion, &mvcc.Write{
				StartTS: req.StartVersion,
				Kind:    lock.Kind,
			})
			txn.DeleteLock(key)
		}
	}
	// 将所有操作写入底层存储
	err = server.storage.Write(req.Context, txn.Writes())
	if err != nil {
		return server.bindResolveErr(resp, err)
	}
	return resp, nil
}

// SQL push down commands.
func (server *Server) Coprocessor(_ context.Context, req *coppb.Request) (*coppb.Response, error) {
	resp := new(coppb.Response)
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			resp.RegionError = regionErr.RequestErr
			return resp, nil
		}
		return nil, err
	}
	switch req.Tp {
	case kv.ReqTypeDAG:
		return server.copHandler.HandleCopDAGRequest(reader, req), nil
	case kv.ReqTypeAnalyze:
		return server.copHandler.HandleCopAnalyzeRequest(reader, req), nil
	}
	return nil, nil
}
