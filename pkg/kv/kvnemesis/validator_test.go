// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package kvnemesis

import (
	"context"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var retryableError = roachpb.NewTransactionRetryWithProtoRefreshError(
	``, uuid.MakeV4(), roachpb.Transaction{})

func withTimestamp(op Operation, ts int) Operation {
	op.Result().OptionalTimestamp = hlc.Timestamp{WallTime: int64(ts)}
	return op
}

func withResultTS(op Operation, ts int) Operation {
	return withTimestamp(withResultOK(op), ts)
}

func withResultOK(op Operation) Operation {
	return withResult(op)
}

func withResult(op Operation) Operation {
	return withResultErr(op, nil /* err */)
}

func withResultErr(op Operation, err error) Operation {
	*op.Result() = resultInit(context.Background(), err)
	// Most operations in tests use timestamp 1, so use that and any test cases
	// that differ from that can use withTimestamp().
	if op.Result().OptionalTimestamp.IsEmpty() {
		op.Result().OptionalTimestamp = hlc.Timestamp{WallTime: 1}
	}
	return op
}

func withReadResult(op Operation, value string) Operation {
	op = withResult(op)
	get := op.GetValue().(*GetOperation)
	get.Result.Type = ResultType_Value
	if value != `` {
		get.Result.Value = roachpb.MakeValueFromString(value).RawBytes
	}
	return op
}

func withScanResult(op Operation, kvs ...KeyValue) Operation {
	op = withResult(op)
	scan := op.GetValue().(*ScanOperation)
	scan.Result.Type = ResultType_Values
	scan.Result.Values = kvs
	return op
}

func withDeleteRangeResult(op Operation, keys ...[]byte) Operation {
	op = withResult(op)
	delRange := op.GetValue().(*DeleteRangeOperation)
	delRange.Result.Type = ResultType_Keys
	delRange.Result.Keys = keys
	return op
}

func TestValidate(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	kv := func(key string, ts int, value string) storage.MVCCKeyValue {
		return storage.MVCCKeyValue{
			Key: storage.MVCCKey{
				Key:       []byte(key),
				Timestamp: hlc.Timestamp{WallTime: int64(ts)},
			},
			Value: roachpb.MakeValueFromString(value).RawBytes,
		}
	}
	tombstone := func(key string, ts int) storage.MVCCKeyValue {
		return storage.MVCCKeyValue{
			Key: storage.MVCCKey{
				Key:       []byte(key),
				Timestamp: hlc.Timestamp{WallTime: int64(ts)},
			},
			Value: nil,
		}
	}
	kvs := func(kvs ...storage.MVCCKeyValue) []storage.MVCCKeyValue {
		return kvs
	}
	scanKV := func(key, value string) KeyValue {
		return KeyValue{
			Key:   []byte(key),
			Value: roachpb.MakeValueFromString(value).RawBytes,
		}
	}

	tests := []struct {
		name     string
		steps    []Step
		kvs      []storage.MVCCKeyValue
		expected []string
	}{
		{
			name:     "no ops and no kvs",
			steps:    nil,
			kvs:      nil,
			expected: nil,
		},
		{
			name:     "no ops with unexpected write",
			steps:    nil,
			kvs:      kvs(kv(`a`, 1, `v1`)),
			expected: []string{`extra writes: [w]"a":0.000000001,0->v1`},
		},
		{
			name:     "no ops with unexpected delete",
			steps:    nil,
			kvs:      kvs(tombstone(`a`, 1)),
			expected: []string{`extra writes: [d]"a":uncertain-><nil>`},
		},
		{
			name:     "one put with expected write",
			steps:    []Step{step(withResult(put(`a`, `v1`)))},
			kvs:      kvs(kv(`a`, 1, `v1`)),
			expected: nil,
		},
		{
			name:     "one delete with expected write",
			steps:    []Step{step(withResult(del(`a`)))},
			kvs:      kvs(tombstone(`a`, 1)),
			expected: nil,
		},
		{
			name:     "one put with missing write",
			steps:    []Step{step(withResult(put(`a`, `v1`)))},
			kvs:      nil,
			expected: []string{`committed put missing write: [w]"a":missing->v1`},
		},
		{
			name:     "one delete with missing write",
			steps:    []Step{step(withResult(del(`a`)))},
			kvs:      nil,
			expected: []string{`committed delete missing write: [d]"a":missing-><nil>`},
		},
		{
			name:     "one ambiguous put with successful write",
			steps:    []Step{step(withResultErr(put(`a`, `v1`), roachpb.NewAmbiguousResultError(errors.New("boom"))))},
			kvs:      kvs(kv(`a`, 1, `v1`)),
			expected: nil,
		},
		{
			name:     "one ambiguous delete with successful write",
			steps:    []Step{step(withResultErr(del(`a`), roachpb.NewAmbiguousResultError(errors.New("boom"))))},
			kvs:      kvs(tombstone(`a`, 1)),
			expected: []string{`unable to validate delete operations in ambiguous transactions: [d]"a":missing-><nil>`},
		},
		{
			name:     "one ambiguous put with failed write",
			steps:    []Step{step(withResultErr(put(`a`, `v1`), roachpb.NewAmbiguousResultError(errors.New("boom"))))},
			kvs:      nil,
			expected: nil,
		},
		{
			name:     "one ambiguous delete with failed write",
			steps:    []Step{step(withResultErr(del(`a`), roachpb.NewAmbiguousResultError(errors.New("boom"))))},
			kvs:      nil,
			expected: nil,
		},
		{
			name: "one ambiguous delete with failed write before a later committed delete",
			steps: []Step{
				step(withResultErr(del(`a`), roachpb.NewAmbiguousResultError(errors.New("boom")))),
				step(withResultTS(del(`a`), 2)),
			},
			kvs: kvs(tombstone(`a`, 2)),
			expected: []string{
				`unable to validate delete operations in ambiguous transactions: [d]"a":missing-><nil>`,
			},
		},
		{
			name:     "one retryable put with write (correctly) missing",
			steps:    []Step{step(withResultErr(put(`a`, `v1`), retryableError))},
			kvs:      nil,
			expected: nil,
		},
		{
			name:     "one retryable delete with write (correctly) missing",
			steps:    []Step{step(withResultErr(del(`a`), retryableError))},
			kvs:      nil,
			expected: nil,
		},
		{
			name:     "one retryable put with write (incorrectly) present",
			steps:    []Step{step(withResultErr(put(`a`, `v1`), retryableError))},
			kvs:      kvs(kv(`a`, 1, `v1`)),
			expected: []string{`uncommitted put had writes: [w]"a":0.000000001,0->v1`},
		},
		{
			name:  "one retryable delete with write (incorrectly) present",
			steps: []Step{step(withResultErr(del(`a`), retryableError))},
			kvs:   kvs(tombstone(`a`, 1)),
			// NB: Error messages are different because we can't match an uncommitted
			// delete op to a stored kv like above.
			expected: []string{`extra writes: [d]"a":uncertain-><nil>`},
		},
		{
			name: "one delete with expected write after write transaction with shadowed delete",
			steps: []Step{
				step(withResultTS(del(`a`), 1)),
				step(withResultTS(put(`a`, `v1`), 2)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withResultOK(put(`a`, `v2`)),
					withResultOK(del(`a`)),
					withResultOK(put(`a`, `v3`)),
				), 3)),
				step(withResultTS(del(`a`), 4)),
			},
			kvs: kvs(
				tombstone(`a`, 1),
				kv(`a`, 2, `v1`),
				kv(`a`, 3, `v3`),
				tombstone(`a`, 4)),
			expected: nil,
		},
		{
			name:     "one batch put with successful write",
			steps:    []Step{step(withResult(batch(withResult(put(`a`, `v1`)))))},
			kvs:      kvs(kv(`a`, 1, `v1`)),
			expected: nil,
		},
		{
			name:     "one batch delete with successful write",
			steps:    []Step{step(withResult(batch(withResult(del(`a`)))))},
			kvs:      kvs(tombstone(`a`, 1)),
			expected: nil,
		},
		{
			name:     "one batch put with missing write",
			steps:    []Step{step(withResult(batch(withResult(put(`a`, `v1`)))))},
			kvs:      nil,
			expected: []string{`committed batch missing write: [w]"a":missing->v1`},
		},
		{
			name:     "one batch delete with missing write",
			steps:    []Step{step(withResult(batch(withResult(del(`a`)))))},
			kvs:      nil,
			expected: []string{`committed batch missing write: [d]"a":missing-><nil>`},
		},
		{
			name: "one transactionally committed put with the correct writes",
			steps: []Step{
				step(withResult(withTimestamp(closureTxn(ClosureTxnType_Commit,
					withResult(put(`a`, `v1`)),
				), 1))),
			},
			kvs:      kvs(kv(`a`, 1, `v1`)),
			expected: nil,
		},
		{
			name: "one transactionally committed delete with the correct writes",
			steps: []Step{
				step(withResult(withTimestamp(closureTxn(ClosureTxnType_Commit,
					withResult(del(`a`)),
				), 1))),
			},
			kvs:      kvs(tombstone(`a`, 1)),
			expected: nil,
		},
		{
			name: "one transactionally committed put with first write missing",
			steps: []Step{
				step(withResult(withTimestamp(closureTxn(ClosureTxnType_Commit,
					withResult(put(`a`, `v1`)),
					withResult(put(`b`, `v2`)),
				), 1))),
			},
			kvs:      kvs(kv(`b`, 1, `v2`)),
			expected: []string{`committed txn missing write: [w]"a":missing->v1 [w]"b":0.000000001,0->v2`},
		},
		{
			name: "one transactionally committed delete with first write missing",
			steps: []Step{
				step(withResult(withTimestamp(closureTxn(ClosureTxnType_Commit,
					withResult(del(`a`)),
					withResult(del(`b`)),
				), 1))),
			},
			kvs:      kvs(tombstone(`b`, 1)),
			expected: []string{`committed txn missing write: [d]"a":missing-><nil> [d]"b":0.000000001,0-><nil>`},
		},
		{
			name: "one transactionally committed put with second write missing",
			steps: []Step{
				step(withResult(withTimestamp(closureTxn(ClosureTxnType_Commit,
					withResult(put(`a`, `v1`)),
					withResult(put(`b`, `v2`)),
				), 1))),
			},
			kvs:      kvs(kv(`a`, 1, `v1`)),
			expected: []string{`committed txn missing write: [w]"a":0.000000001,0->v1 [w]"b":missing->v2`},
		},
		{
			name: "one transactionally committed delete with second write missing",
			steps: []Step{
				step(withResult(withTimestamp(closureTxn(ClosureTxnType_Commit,
					withResult(del(`a`)),
					withResult(del(`b`)),
				), 1))),
			},
			kvs:      kvs(tombstone(`a`, 1)),
			expected: []string{`committed txn missing write: [d]"a":0.000000001,0-><nil> [d]"b":missing-><nil>`},
		},
		{
			name: "one transactionally committed put with write timestamp disagreement",
			steps: []Step{
				step(withResult(withTimestamp(closureTxn(ClosureTxnType_Commit,
					withResult(put(`a`, `v1`)),
					withResult(put(`b`, `v2`)),
				), 1))),
			},
			kvs: kvs(kv(`a`, 1, `v1`), kv(`b`, 2, `v2`)),
			expected: []string{
				`committed txn non-atomic timestamps: [w]"a":0.000000001,0->v1 [w]"b":0.000000002,0->v2`,
			},
		},
		{
			name: "one transactionally committed delete with write timestamp disagreement",
			steps: []Step{
				step(withResult(withTimestamp(closureTxn(ClosureTxnType_Commit,
					withResult(del(`a`)),
					withResult(del(`b`)),
				), 1))),
			},
			kvs: kvs(tombstone(`a`, 1), tombstone(`b`, 2)),
			// NB: Error messages are different because we can't match an uncommitted
			// delete op to a stored kv like above.
			expected: []string{
				`committed txn missing write: [d]"a":0.000000001,0-><nil> [d]"b":missing-><nil>`,
			},
		},
		{
			name: "one transactionally rolled back put with write (correctly) missing",
			steps: []Step{
				step(withResultErr(closureTxn(ClosureTxnType_Rollback,
					withResult(put(`a`, `v1`)),
				), errors.New(`rollback`))),
			},
			kvs:      nil,
			expected: nil,
		},
		{
			name: "one transactionally rolled back delete with write (correctly) missing",
			steps: []Step{
				step(withResultErr(closureTxn(ClosureTxnType_Rollback,
					withResult(del(`a`)),
				), errors.New(`rollback`))),
			},
			kvs:      nil,
			expected: nil,
		},
		{
			name: "one transactionally rolled back put with write (incorrectly) present",
			steps: []Step{
				step(withResultErr(closureTxn(ClosureTxnType_Rollback,
					withResult(put(`a`, `v1`)),
				), errors.New(`rollback`))),
			},
			kvs:      kvs(kv(`a`, 1, `v1`)),
			expected: []string{`uncommitted txn had writes: [w]"a":0.000000001,0->v1`},
		},
		{
			name: "one transactionally rolled back delete with write (incorrectly) present",
			steps: []Step{
				step(withResultErr(closureTxn(ClosureTxnType_Rollback,
					withResult(del(`a`)),
				), errors.New(`rollback`))),
			},
			kvs:      kvs(tombstone(`a`, 1)),
			expected: []string{`extra writes: [d]"a":uncertain-><nil>`},
		},
		{
			name: "one transactionally rolled back batch put with write (correctly) missing",
			steps: []Step{
				step(withResultErr(closureTxn(ClosureTxnType_Rollback,
					withResult(batch(
						withResult(put(`a`, `v1`)),
					)),
				), errors.New(`rollback`))),
			},
			kvs:      nil,
			expected: nil,
		},
		{
			name: "one transactionally rolled back batch delete with write (correctly) missing",
			steps: []Step{
				step(withResultErr(closureTxn(ClosureTxnType_Rollback,
					withResult(batch(
						withResult(del(`a`)),
					)),
				), errors.New(`rollback`))),
			},
			kvs:      nil,
			expected: nil,
		},
		{
			name: "two transactionally committed puts of the same key",
			steps: []Step{
				step(withResult(withTimestamp(closureTxn(ClosureTxnType_Commit,
					withResult(put(`a`, `v1`)),
					withResult(put(`a`, `v2`)),
				), 1))),
			},
			kvs:      kvs(kv(`a`, 1, `v2`)),
			expected: nil,
		},
		{
			name: "two transactionally committed deletes of the same key",
			steps: []Step{
				step(withResult(withTimestamp(closureTxn(ClosureTxnType_Commit,
					withResult(del(`a`)),
					withResult(del(`a`)),
				), 1))),
			},
			kvs:      kvs(tombstone(`a`, 1)),
			expected: nil,
		},
		{
			name: "two transactionally committed writes (put, delete) of the same key",
			steps: []Step{
				step(withResult(withTimestamp(closureTxn(ClosureTxnType_Commit,
					withResult(put(`a`, `v1`)),
					withResult(del(`a`)),
				), 1))),
			},
			kvs:      kvs(tombstone(`a`, 1)),
			expected: nil,
		},
		{
			name: "two transactionally committed writes (delete, put) of the same key",
			steps: []Step{
				step(withResult(withTimestamp(closureTxn(ClosureTxnType_Commit,
					withResult(del(`a`)),
					withResult(put(`a`, `v2`)),
				), 1))),
			},
			kvs:      kvs(kv(`a`, 1, `v2`)),
			expected: nil,
		},
		{
			name: "two transactionally committed puts of the same key with extra write",
			steps: []Step{
				step(withResult(withTimestamp(closureTxn(ClosureTxnType_Commit,
					withResult(put(`a`, `v1`)),
					withResult(put(`a`, `v2`)),
				), 2))),
			},
			// HACK: These should be the same timestamp. See the TODO in
			// watcher.processEvents.
			kvs: kvs(kv(`a`, 1, `v1`), kv(`a`, 2, `v2`)),
			expected: []string{
				`committed txn overwritten key had write: [w]"a":0.000000001,0->v1 [w]"a":0.000000002,0->v2`,
			},
		},
		{
			name: "two transactionally committed deletes of the same key with extra write",
			steps: []Step{
				step(withResult(withTimestamp(closureTxn(ClosureTxnType_Commit,
					withResult(del(`a`)),
					withResult(del(`a`)),
				), 1))),
			},
			// HACK: These should be the same timestamp. See the TODO in
			// watcher.processEvents.
			kvs:      kvs(tombstone(`a`, 1), tombstone(`a`, 2)),
			expected: []string{`extra writes: [d]"a":uncertain-><nil>`},
		},
		{
			name: "two transactionally committed writes (put, delete) of the same key with extra write",
			steps: []Step{
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withResultOK(put(`a`, `v1`)),
					withResultOK(del(`a`)),
				), 1)),
			},
			// HACK: These should be the same timestamp. See the TODO in
			// watcher.processEvents.
			kvs: kvs(kv(`a`, 1, `v1`), tombstone(`a`, 2)),
			expected: []string{
				// NB: the deletion is marked as "missing" because we are using timestamp 1 for the
				// txn and the tombstone is at 2; so it isn't marked as materialized in the verifier.
				`committed txn overwritten key had write: [w]"a":0.000000001,0->v1 [d]"a":missing-><nil>`,
			},
		},
		{
			name: "ambiguous transaction committed",
			steps: []Step{
				step(withResultErr(closureTxn(ClosureTxnType_Commit,
					withResult(put(`a`, `v1`)),
					withResult(put(`b`, `v2`)),
				), roachpb.NewAmbiguousResultError(errors.New("boom")))),
			},
			kvs:      kvs(kv(`a`, 1, `v1`), kv(`b`, 1, `v2`)),
			expected: nil,
		},
		{
			name: "ambiguous transaction with delete committed",
			steps: []Step{
				step(withResultErr(closureTxn(ClosureTxnType_Commit,
					withResult(put(`a`, `v1`)),
					withResult(del(`b`)),
				), roachpb.NewAmbiguousResultError(errors.New("boom")))),
			},
			kvs: kvs(kv(`a`, 1, `v1`), tombstone(`b`, 1)),
			// TODO(sarkesian): If able to determine the tombstone resulting from a
			// delete in an ambiguous txn, this should pass without error.
			// For now we fail validation on all ambiguous transactions with deletes.
			expected: []string{
				`unable to validate delete operations in ambiguous transactions: [w]"a":0.000000001,0->v1 [d]"b":missing-><nil>`,
			},
		},
		{
			name: "ambiguous transaction did not commit",
			steps: []Step{
				step(withResultErr(closureTxn(ClosureTxnType_Commit,
					withResult(put(`a`, `v1`)),
					withResult(put(`b`, `v2`)),
				), roachpb.NewAmbiguousResultError(errors.New("boom")))),
			},
			kvs:      nil,
			expected: nil,
		},
		{
			name: "ambiguous transaction with delete did not commit",
			steps: []Step{
				step(withResultErr(closureTxn(ClosureTxnType_Commit,
					withResult(put(`a`, `v1`)),
					withResult(del(`b`)),
				), roachpb.NewAmbiguousResultError(errors.New("boom")))),
			},
			kvs:      nil,
			expected: nil,
		},
		{
			name: "ambiguous transaction committed but has validation error",
			steps: []Step{
				step(withResultErr(closureTxn(ClosureTxnType_Commit,
					withResult(put(`a`, `v1`)),
					withResult(put(`b`, `v2`)),
				), roachpb.NewAmbiguousResultError(errors.New("boom")))),
			},
			kvs: kvs(kv(`a`, 1, `v1`), kv(`b`, 2, `v2`)),
			expected: []string{
				`ambiguous txn non-atomic timestamps: [w]"a":0.000000001,0->v1 [w]"b":0.000000002,0->v2`,
			},
		},
		{
			name: "ambiguous transaction with delete committed but has validation error",
			steps: []Step{
				step(withResultErr(withTimestamp(closureTxn(ClosureTxnType_Commit,
					withResult(put(`a`, `v1`)),
					withResult(del(`b`)),
				), 2), roachpb.NewAmbiguousResultError(errors.New("boom")))),
			},
			kvs: kvs(kv(`a`, 1, `v1`), tombstone(`b`, 2)),
			// TODO(sarkesian): If able to determine the tombstone resulting from a
			// delete in an ambiguous txn, we should get the following error:
			// `ambiguous txn non-atomic timestamps: [w]"a":0.000000001,0->v1 [w]"b":0.000000002,0->v2`
			// For now we fail validation on all ambiguous transactions with deletes.
			expected: []string{
				`unable to validate delete operations in ambiguous transactions: [w]"a":0.000000001,0->v1 [d]"b":missing-><nil>`,
			},
		},
		{
			name: "one read before write",
			steps: []Step{
				step(withReadResult(get(`a`), ``)),
				step(withResult(put(`a`, `v1`))),
			},
			kvs:      kvs(kv(`a`, 1, `v1`)),
			expected: nil,
		},
		{
			name: "one read before delete",
			steps: []Step{
				step(withReadResult(get(`a`), ``)),
				step(withResult(del(`a`))),
			},
			kvs:      kvs(tombstone(`a`, 1)),
			expected: nil,
		},
		{
			name: "one read before write and delete",
			steps: []Step{
				step(withReadResult(get(`a`), ``)),
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(del(`a`), 2)),
			},
			kvs:      kvs(kv(`a`, 1, `v1`), tombstone(`a`, 2)),
			expected: nil,
		},
		{
			name: "one read before write returning wrong value",
			steps: []Step{
				step(withReadResult(get(`a`), `v2`)),
				step(withResult(put(`a`, `v1`))),
			},
			kvs: kvs(kv(`a`, 1, `v1`)),
			expected: []string{
				`committed get non-atomic timestamps: [r]"a":[0,0, 0,0)->v2`,
			},
		},
		{
			name: "one read after write",
			steps: []Step{
				step(withResult(put(`a`, `v1`))),
				step(withReadResult(get(`a`), `v1`)),
			},
			kvs:      kvs(kv(`a`, 1, `v1`)),
			expected: nil,
		},
		{
			name: "one read after write and delete",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(withTimestamp(del(`a`), 2), 2)),
				step(withResultTS(withReadResult(get(`a`), `v1`), 1)),
			},
			kvs:      kvs(kv(`a`, 1, `v1`), tombstone(`a`, 2)),
			expected: nil,
		},
		{
			name: "one read after write and delete returning tombstone",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(del(`a`), 2)),
				step(withReadResult(get(`a`), ``)),
			},
			kvs:      kvs(kv(`a`, 1, `v1`), tombstone(`a`, 2)),
			expected: nil,
		},
		{
			name: "one read after write returning wrong value",
			steps: []Step{
				step(withResult(put(`a`, `v1`))),
				step(withReadResult(get(`a`), `v2`)),
			},
			kvs: kvs(kv(`a`, 1, `v1`)),
			expected: []string{
				`committed get non-atomic timestamps: [r]"a":[0,0, 0,0)->v2`,
			},
		},
		{
			name: "one read in between writes",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withReadResult(get(`a`), `v1`)),
				step(withResultTS(put(`a`, `v2`), 2)),
			},
			kvs:      kvs(kv(`a`, 1, `v1`), kv(`a`, 2, `v2`)),
			expected: nil,
		},
		{
			name: "one read in between write and delete",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withReadResult(get(`a`), `v1`)),
				step(withResultTS(del(`a`), 2)),
			},
			kvs:      kvs(kv(`a`, 1, `v1`), tombstone(`a`, 2)),
			expected: nil,
		},
		{
			name: "batch of reads after writes",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`b`, `v2`), 2)),
				step(withResult(batch(
					withReadResult(get(`a`), `v1`),
					withReadResult(get(`b`), `v2`),
					withReadResult(get(`c`), ``),
				))),
			},
			kvs:      kvs(kv(`a`, 1, `v1`), kv(`b`, 2, `v2`)),
			expected: nil,
		},
		{
			name: "batch of reads after writes and deletes",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`b`, `v2`), 2)),
				step(withResultTS(del(`a`), 3)),
				step(withResultTS(del(`b`), 4)),
				step(withResult(batch(
					withReadResult(get(`a`), `v1`),
					withReadResult(get(`b`), `v2`),
					withReadResult(get(`c`), ``),
				))),
			},
			kvs:      kvs(kv(`a`, 1, `v1`), kv(`b`, 2, `v2`), tombstone(`a`, 3), tombstone(`b`, 4)),
			expected: nil,
		},
		{
			name: "batch of reads after writes and deletes returning tombstones",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`b`, `v2`), 2)),
				step(withResultTS(del(`a`), 3)),
				step(withResultTS(del(`b`), 4)),
				step(withResult(batch(
					withReadResult(get(`a`), ``),
					withReadResult(get(`b`), ``),
					withReadResult(get(`c`), ``),
				))),
			},
			kvs:      kvs(kv(`a`, 1, `v1`), kv(`b`, 2, `v2`), tombstone(`a`, 3), tombstone(`b`, 4)),
			expected: nil,
		},
		{
			name: "batch of reads after writes returning wrong values",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`b`, `v2`), 2)),
				step(withResult(batch(
					withReadResult(get(`a`), ``),
					withReadResult(get(`b`), `v1`),
					withReadResult(get(`c`), `v2`),
				))),
			},
			kvs: kvs(kv(`a`, 1, `v1`), kv(`b`, 2, `v2`)),
			expected: []string{
				`committed batch non-atomic timestamps: ` +
					`[r]"a":[<min>, 0.000000001,0)-><nil> [r]"b":[0,0, 0,0)->v1 [r]"c":[0,0, 0,0)->v2`,
			},
		},
		{
			name: "batch of reads after writes and deletes returning wrong values",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`b`, `v2`), 2)),
				step(withResultTS(del(`a`), 3)),
				step(withResultTS(del(`b`), 4)),
				step(withResult(batch(
					withReadResult(get(`a`), ``),
					withReadResult(get(`b`), `v1`),
					withReadResult(get(`c`), `v2`),
				))),
			},
			kvs: kvs(kv(`a`, 1, `v1`), kv(`b`, 2, `v2`), tombstone(`a`, 3), tombstone(`b`, 4)),
			expected: []string{
				`committed batch non-atomic timestamps: ` +
					`[r]"a":[<min>, 0.000000001,0),[0.000000003,0, <max>)-><nil> [r]"b":[0,0, 0,0)->v1 [r]"c":[0,0, 0,0)->v2`,
			},
		},
		{
			name: "batch of reads after writes with non-empty time overlap",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`b`, `v2`), 2)),
				step(withResult(batch(
					withReadResult(get(`a`), ``),
					withReadResult(get(`b`), `v2`),
					withReadResult(get(`c`), ``),
				))),
			},
			kvs: kvs(kv(`a`, 1, `v1`), kv(`b`, 2, `v2`)),
			expected: []string{
				`committed batch non-atomic timestamps: ` +
					`[r]"a":[<min>, 0.000000001,0)-><nil> [r]"b":[0.000000002,0, <max>)->v2 [r]"c":[<min>, <max>)-><nil>`,
			},
		},
		{
			name: "batch of reads after writes and deletes with valid time overlap",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`b`, `v2`), 2)),
				step(withResultTS(del(`a`), 3)),
				step(withResultTS(del(`b`), 4)),
				step(withResult(batch(
					withReadResult(get(`a`), ``),
					withReadResult(get(`b`), `v2`),
					withReadResult(get(`c`), ``),
				))),
			},
			kvs:      kvs(kv(`a`, 1, `v1`), kv(`b`, 2, `v2`), tombstone(`a`, 3), tombstone(`b`, 4)),
			expected: nil,
		},
		{
			name: "transactional reads with non-empty time overlap",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`a`, `v2`), 3)),
				step(withResultTS(put(`b`, `v3`), 2)),
				step(withResultTS(put(`b`, `v4`), 3)),
				step(withResult(withTimestamp(closureTxn(ClosureTxnType_Commit,
					withReadResult(get(`a`), `v1`),
					withReadResult(get(`b`), `v3`),
				), 3))),
			},
			// Reading v1 is valid from 1-3 and v3 is valid from 2-3: overlap 2-3
			kvs:      kvs(kv(`a`, 1, `v1`), kv(`a`, 3, `v2`), kv(`b`, 2, `v3`), kv(`b`, 3, `v4`)),
			expected: nil,
		},
		{
			name: "transactional reads after writes and deletes with non-empty time overlap",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`b`, `v2`), 2)),
				step(withResultTS(del(`a`), 3)),
				step(withResultTS(del(`b`), 4)),
				step(withResult(withTimestamp(closureTxn(ClosureTxnType_Commit,
					withReadResult(get(`a`), ``),
					withReadResult(get(`b`), `v2`),
					withReadResult(get(`c`), ``),
				), 4))),
			},
			// Reading (a, <nil>) is valid from min-1 or 3-max, and (b, v2) is valid from 2-4: overlap 3-4
			kvs:      kvs(kv(`a`, 1, `v1`), kv(`b`, 2, `v2`), tombstone(`a`, 3), tombstone(`b`, 4)),
			expected: nil,
		},
		{
			name: "transactional reads with empty time overlap",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`a`, `v2`), 2)),
				step(withResultTS(put(`b`, `v3`), 2)),
				step(withResultTS(put(`b`, `v4`), 3)),
				step(withResult(withTimestamp(closureTxn(ClosureTxnType_Commit,
					withReadResult(get(`a`), `v1`),
					withReadResult(get(`b`), `v3`),
				), 3))),
			},
			// Reading v1 is valid from 1-2 and v3 is valid from 2-3: no overlap
			kvs: kvs(kv(`a`, 1, `v1`), kv(`a`, 2, `v2`), kv(`b`, 2, `v3`), kv(`b`, 3, `v4`)),
			expected: []string{
				`committed txn non-atomic timestamps: ` +
					`[r]"a":[0.000000001,0, 0.000000002,0)->v1 [r]"b":[0.000000002,0, 0.000000003,0)->v3`,
			},
		},
		{
			name: "transactional reads after writes and deletes with empty time overlap",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`b`, `v2`), 2)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withResultOK(del(`a`)),
					withResultOK(del(`b`)),
				), 3)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withReadResult(get(`a`), ``),
					withReadResult(get(`b`), `v2`),
					withReadResult(get(`c`), ``),
				), 4)),
			},
			// Reading (a, <nil>) is valid from min-1 or 3-max, and (b, v2) is valid from 2-3: no overlap
			kvs: kvs(kv(`a`, 1, `v1`), kv(`b`, 2, `v2`), tombstone(`a`, 3), tombstone(`b`, 3)),
			expected: []string{
				`committed txn non-atomic timestamps: ` +
					`[r]"a":[<min>, 0.000000001,0),[0.000000003,0, <max>)-><nil> [r]"b":[0.000000002,0, 0.000000003,0)->v2 [r]"c":[<min>, <max>)-><nil>`,
			},
		},
		{
			name: "transactional reads and deletes after write with non-empty time overlap",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withReadResult(get(`a`), `v1`),
					withResult(del(`a`)),
					withReadResult(get(`a`), ``),
				), 2)),
				step(withResultTS(put(`a`, `v2`), 3)),
				step(withResultTS(del(`a`), 4)),
			},
			// Reading (a, v1) is valid from 1-2, reading (a, <nil>) is valid from min-1, 2-3, or 4-max: overlap in txn view at 2
			kvs:      kvs(kv(`a`, 1, `v1`), tombstone(`a`, 2), kv(`a`, 3, `v2`), tombstone(`a`, 4)),
			expected: nil,
		},
		{
			name: "transactional reads and deletes after write with empty time overlap",
			steps: []Step{
				step(withResult(put(`a`, `v1`))),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withReadResult(get(`a`), ``),
					withResult(del(`a`)),
					withReadResult(get(`a`), ``),
				), 2)),
				step(withResultTS(put(`a`, `v2`), 3)),
				step(withResultTS(del(`a`), 4)),
			},
			// First read of (a, <nil>) is valid from min-1 or 4-max, delete is valid at 2: no overlap
			kvs: kvs(kv(`a`, 1, `v1`), tombstone(`a`, 2), kv(`a`, 3, `v2`), tombstone(`a`, 4)),
			expected: []string{
				`committed txn non-atomic timestamps: ` +
					`[r]"a":[<min>, 0.000000001,0),[0.000000004,0, <max>)-><nil> [d]"a":0.000000002,0-><nil> [r]"a":[<min>, 0.000000001,0),[0.000000004,0, <max>),[0.000000002,0, 0.000000003,0)-><nil>`,
			},
		},
		{
			name: "transactional reads one missing with non-empty time overlap",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`a`, `v2`), 2)),
				step(withResultTS(put(`b`, `v3`), 2)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withReadResult(get(`a`), `v1`),
					withReadResult(get(`b`), ``),
				), 1)),
			},
			// Reading v1 is valid from 1-2 and v3 is valid from 0-2: overlap 1-2
			kvs:      kvs(kv(`a`, 1, `v1`), kv(`a`, 2, `v2`), kv(`b`, 2, `v3`)),
			expected: nil,
		},
		{
			name: "transactional reads one missing with empty time overlap",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`a`, `v2`), 2)),
				step(withResultTS(put(`b`, `v3`), 1)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withReadResult(get(`a`), `v1`),
					withReadResult(get(`b`), ``),
				), 1)),
			},
			// Reading v1 is valid from 1-2 and v3 is valid from 0-1: no overlap
			kvs: kvs(kv(`a`, 1, `v1`), kv(`a`, 2, `v2`), kv(`b`, 1, `v3`)),
			expected: []string{
				`committed txn non-atomic timestamps: ` +
					`[r]"a":[0.000000001,0, 0.000000002,0)->v1 [r]"b":[<min>, 0.000000001,0)-><nil>`,
			},
		},
		{
			name: "transactional read and write with non-empty time overlap",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`a`, `v2`), 3)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withReadResult(get(`a`), `v1`),
					withResult(put(`b`, `v3`)),
				), 2)),
			},
			// Reading v1 is valid from 1-3 and v3 is valid at 2: overlap @2
			kvs:      kvs(kv(`a`, 1, `v1`), kv(`a`, 3, `v2`), kv(`b`, 2, `v3`)),
			expected: nil,
		},
		{
			name: "transactional read and write with empty time overlap",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`a`, `v2`), 2)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withReadResult(get(`a`), `v1`),
					withResultOK(put(`b`, `v3`)),
				), 2)),
			},
			// Reading v1 is valid from 1-2 and v3 is valid at 2: no overlap
			kvs: kvs(kv(`a`, 1, `v1`), kv(`a`, 2, `v2`), kv(`b`, 2, `v3`)),
			expected: []string{
				`committed txn non-atomic timestamps: ` +
					`[r]"a":[0.000000001,0, 0.000000002,0)->v1 [w]"b":0.000000002,0->v3`,
			},
		},
		{
			name: "transaction with read before and after write",
			steps: []Step{
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withReadResult(get(`a`), ``),
					withResult(put(`a`, `v1`)),
					withReadResult(get(`a`), `v1`),
				), 1)),
			},
			kvs:      kvs(kv(`a`, 1, `v1`)),
			expected: nil,
		},
		{
			name: "transaction with read before and after delete",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withReadResult(get(`a`), `v1`),
					withResult(del(`a`)),
					withReadResult(get(`a`), ``),
				), 2)),
			},
			kvs:      kvs(kv(`a`, 1, `v1`), tombstone(`a`, 2)),
			expected: nil,
		},
		{
			name: "transaction with incorrect read before write",
			steps: []Step{
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withReadResult(get(`a`), `v1`),
					withResult(put(`a`, `v1`)),
					withReadResult(get(`a`), `v1`),
				), 1)),
			},
			kvs: kvs(kv(`a`, 1, `v1`)),
			expected: []string{
				`committed txn non-atomic timestamps: ` +
					`[r]"a":[0,0, 0,0)->v1 [w]"a":0.000000001,0->v1 [r]"a":[0.000000001,0, <max>)->v1`,
			},
		},
		{
			name: "transaction with incorrect read before delete",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withReadResult(get(`a`), ``),
					withResult(del(`a`)),
					withReadResult(get(`a`), ``),
				), 2)),
			},
			kvs: kvs(kv(`a`, 1, `v1`), tombstone(`a`, 2)),
			expected: []string{
				`committed txn non-atomic timestamps: ` +
					`[r]"a":[<min>, 0.000000001,0)-><nil> [d]"a":0.000000002,0-><nil> [r]"a":[<min>, 0.000000001,0),[0.000000002,0, <max>)-><nil>`,
			},
		},
		{
			name: "transaction with incorrect read after write",
			steps: []Step{
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withReadResult(get(`a`), ``),
					withResult(put(`a`, `v1`)),
					withReadResult(get(`a`), ``),
				), 1)),
			},
			kvs: kvs(kv(`a`, 1, `v1`)),
			expected: []string{
				`committed txn non-atomic timestamps: ` +
					`[r]"a":[<min>, <max>)-><nil> [w]"a":0.000000001,0->v1 [r]"a":[<min>, 0.000000001,0)-><nil>`,
			},
		},
		{
			name: "transaction with incorrect read after delete",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withReadResult(get(`a`), `v1`),
					withResultOK(del(`a`)),
					withReadResult(get(`a`), `v1`),
				), 2)),
			},
			kvs: kvs(kv(`a`, 1, `v1`), tombstone(`a`, 2)),
			expected: []string{
				`committed txn non-atomic timestamps: ` +
					`[r]"a":[0.000000001,0, <max>)->v1 [d]"a":0.000000002,0-><nil> [r]"a":[0.000000001,0, 0.000000002,0)->v1`,
			},
		},
		{
			name: "two transactionally committed puts of the same key with reads",
			steps: []Step{
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withReadResult(get(`a`), ``),
					withResult(put(`a`, `v1`)),
					withReadResult(get(`a`), `v1`),
					withResult(put(`a`, `v2`)),
					withReadResult(get(`a`), `v2`),
				), 1)),
			},
			kvs:      kvs(kv(`a`, 1, `v2`)),
			expected: nil,
		},
		{
			name: "two transactionally committed put/delete ops of the same key with reads",
			steps: []Step{
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withReadResult(get(`a`), ``),
					withResult(put(`a`, `v1`)),
					withReadResult(get(`a`), `v1`),
					withResult(del(`a`)),
					withReadResult(get(`a`), ``),
				), 1)),
			},
			kvs:      kvs(tombstone(`a`, 1)),
			expected: nil,
		},
		{
			name: "two transactionally committed put/delete ops of the same key with incorrect read",
			steps: []Step{
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withReadResult(get(`a`), ``),
					withResult(put(`a`, `v1`)),
					withReadResult(get(`a`), `v1`),
					withResult(del(`a`)),
					withReadResult(get(`a`), `v1`),
				), 1)),
			},
			kvs: kvs(tombstone(`a`, 1)),
			expected: []string{
				`committed txn non-atomic timestamps: ` +
					`[r]"a":[<min>, <max>)-><nil> [w]"a":missing->v1 [r]"a":[0.000000001,0, <max>)->v1 [d]"a":0.000000001,0-><nil> [r]"a":[0,0, 0,0)->v1`,
			},
		},
		{
			name: "one transactional put with correct commit time",
			steps: []Step{
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withResult(put(`a`, `v1`)),
				), 1)),
			},
			kvs:      kvs(kv(`a`, 1, `v1`)),
			expected: nil,
		},
		{
			name: "one transactional put with incorrect commit time",
			steps: []Step{
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withResult(put(`a`, `v1`)),
				), 1)),
			},
			kvs: kvs(kv(`a`, 2, `v1`)),
			expected: []string{
				`mismatched write timestamp 0.000000001,0: [w]"a":0.000000002,0->v1`,
			},
		},
		{
			name: "one transactional delete with write on another key after delete",
			steps: []Step{
				// NB: this Delete comes first in operation order, but the write is delayed.
				step(withResultTS(del(`a`), 3)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withResult(put(`b`, `v1`)),
					withResult(del(`a`)),
				), 2)),
			},
			kvs: kvs(tombstone(`a`, 2), tombstone(`a`, 3), kv(`b`, 2, `v1`)),
			// This should fail validation if we match delete operations to tombstones by operation order,
			// and should pass if we correctly use the transaction timestamp. While the first delete is
			// an earlier operation, the transactional delete actually commits first.
			expected: nil,
		},
		{
			name: "two transactional deletes with out of order commit times",
			steps: []Step{
				step(withResultTS(del(`a`), 2)),
				step(withResultTS(del(`b`), 3)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withResult(del(`a`)),
					withResult(del(`b`)),
				), 1)),
			},
			kvs: kvs(tombstone(`a`, 1), tombstone(`a`, 2), tombstone(`b`, 1), tombstone(`b`, 3)),
			// This should fail validation if we match delete operations to tombstones by operation order,
			// and should pass if we correctly use the transaction timestamp. While the first two deletes are
			// earlier operations, the transactional deletes actually commits first.
			expected: nil,
		},
		{
			name: "one transactional scan followed by delete within time range",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withScanResult(scan(`a`, `c`), scanKV(`a`, `v1`)),
					withResult(del(`a`)),
				), 2)),
				step(withResultTS(put(`b`, `v2`), 3)),
			},
			kvs:      kvs(kv(`a`, 1, `v1`), tombstone(`a`, 2), kv(`b`, 3, `v2`)),
			expected: nil,
		},
		{
			name: "one transactional scan followed by delete outside time range",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withScanResult(scan(`a`, `c`), scanKV(`a`, `v1`)),
					withResult(del(`a`)),
				), 4)),
				step(withResultTS(put(`b`, `v2`), 3)),
			},
			kvs: kvs(kv(`a`, 1, `v1`), tombstone(`a`, 4), kv(`b`, 3, `v2`)),
			expected: []string{
				`committed txn non-atomic timestamps: ` +
					`[s]{a-c}:{0:[0.000000001,0, <max>), gap:[<min>, 0.000000003,0)}->["a":v1] [d]"a":0.000000004,0-><nil>`,
			},
		},
		{
			name: "one scan before write",
			steps: []Step{
				step(withScanResult(scan(`a`, `c`))),
				step(withResult(put(`a`, `v1`))),
			},
			kvs:      kvs(kv(`a`, 1, `v1`)),
			expected: nil,
		},
		{
			name: "one scan before write returning wrong value",
			steps: []Step{
				step(withScanResult(scan(`a`, `c`), scanKV(`a`, `v2`))),
				step(withResult(put(`a`, `v1`))),
			},
			kvs: kvs(kv(`a`, 1, `v1`)),
			expected: []string{
				`committed scan non-atomic timestamps: ` +
					`[s]{a-c}:{0:[0,0, 0,0), gap:[<min>, <max>)}->["a":v2]`,
			},
		},
		{
			name: "one scan after write",
			steps: []Step{
				step(withResult(put(`a`, `v1`))),
				step(withScanResult(scan(`a`, `c`), scanKV(`a`, `v1`))),
			},
			kvs:      kvs(kv(`a`, 1, `v1`)),
			expected: nil,
		},
		{
			name: "one scan after write returning wrong value",
			steps: []Step{
				step(withResult(put(`a`, `v1`))),
				step(withScanResult(scan(`a`, `c`), scanKV(`a`, `v2`))),
			},
			kvs: kvs(kv(`a`, 1, `v1`)),
			expected: []string{
				`committed scan non-atomic timestamps: ` +
					`[s]{a-c}:{0:[0,0, 0,0), gap:[<min>, <max>)}->["a":v2]`,
			},
		},
		{
			name: "one scan after writes",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`b`, `v2`), 2)),
				step(withScanResult(scan(`a`, `c`), scanKV(`a`, `v1`), scanKV(`b`, `v2`))),
			},
			kvs:      kvs(kv(`a`, 1, `v1`), kv(`b`, 2, `v2`)),
			expected: nil,
		},
		{
			name: "one reverse scan after writes",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`b`, `v2`), 2)),
				step(withScanResult(reverseScan(`a`, `c`), scanKV(`b`, `v2`), scanKV(`a`, `v1`))),
			},
			kvs:      kvs(kv(`a`, 1, `v1`), kv(`b`, 2, `v2`)),
			expected: nil,
		},
		{
			name: "one scan after writes and delete",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`b`, `v2`), 2)),
				step(withResultTS(del(`a`), 3)),
				step(withResultTS(put(`a`, `v3`), 4)),
				step(withScanResult(scan(`a`, `c`), scanKV(`b`, `v2`))),
			},
			kvs:      kvs(kv(`a`, 1, `v1`), kv(`b`, 2, `v2`), tombstone(`a`, 3), kv(`a`, 4, `v3`)),
			expected: nil,
		},
		{
			name: "one scan after write returning extra key",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`b`, `v2`), 2)),
				step(withScanResult(scan(`a`, `c`), scanKV(`a`, `v1`), scanKV(`a2`, `v3`), scanKV(`b`, `v2`))),
			},
			kvs: kvs(kv(`a`, 1, `v1`), kv(`b`, 2, `v2`)),
			expected: []string{
				`committed scan non-atomic timestamps: ` +
					`[s]{a-c}:{0:[0.000000001,0, <max>), 1:[0,0, 0,0), 2:[0.000000002,0, <max>), gap:[<min>, <max>)}->["a":v1, "a2":v3, "b":v2]`,
			},
		},
		{
			name: "one tranactional scan after write and delete returning extra key",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withResult(put(`b`, `v2`)),
					withResult(del(`a`)),
				), 2)),
				step(withScanResult(scan(`a`, `c`), scanKV(`a`, `v1`), scanKV(`b`, `v2`))),
			},
			kvs: kvs(kv(`a`, 1, `v1`), tombstone(`a`, 2), kv(`b`, 2, `v2`)),
			expected: []string{
				`committed scan non-atomic timestamps: ` +
					`[s]{a-c}:{0:[0.000000001,0, 0.000000002,0), 1:[0.000000002,0, <max>), gap:[<min>, <max>)}->["a":v1, "b":v2]`,
			},
		},
		{
			name: "one reverse scan after write returning extra key",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`b`, `v2`), 2)),
				step(withScanResult(reverseScan(`a`, `c`), scanKV(`b`, `v2`), scanKV(`a2`, `v3`), scanKV(`a`, `v1`))),
			},
			kvs: kvs(kv(`a`, 1, `v1`), kv(`b`, 2, `v2`)),
			expected: []string{
				`committed reverse scan non-atomic timestamps: ` +
					`[rs]{a-c}:{0:[0.000000002,0, <max>), 1:[0,0, 0,0), 2:[0.000000001,0, <max>), gap:[<min>, <max>)}->["b":v2, "a2":v3, "a":v1]`,
			},
		},
		{
			name: "one scan after write returning missing key",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`b`, `v2`), 2)),
				step(withScanResult(scan(`a`, `c`), scanKV(`b`, `v2`))),
			},
			kvs: kvs(kv(`a`, 1, `v1`), kv(`b`, 2, `v2`)),
			expected: []string{
				`committed scan non-atomic timestamps: ` +
					`[s]{a-c}:{0:[0.000000002,0, <max>), gap:[<min>, 0.000000001,0)}->["b":v2]`,
			},
		},
		{
			name: "one scan after writes and delete returning missing key",
			steps: []Step{
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withResult(put(`a`, `v1`)),
					withResult(put(`b`, `v2`)),
				), 1)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withScanResult(scan(`a`, `c`), scanKV(`b`, `v2`)),
					withResult(del(`a`)),
				), 2)),
				step(withResultTS(put(`a`, `v3`), 3)),
				step(withResultTS(del(`a`), 4)),
			},
			kvs: kvs(kv(`a`, 1, `v1`), kv(`b`, 1, `v2`), tombstone(`a`, 2), kv(`a`, 3, `v3`), tombstone(`a`, 4)),
			expected: []string{
				`committed txn non-atomic timestamps: ` +
					`[s]{a-c}:{0:[0.000000001,0, <max>), gap:[<min>, 0.000000001,0),[0.000000004,0, <max>)}->["b":v2] [d]"a":0.000000002,0-><nil>`,
			},
		},
		{
			name: "one reverse scan after write returning missing key",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`b`, `v2`), 2)),
				step(withScanResult(reverseScan(`a`, `c`), scanKV(`b`, `v2`))),
			},
			kvs: kvs(kv(`a`, 1, `v1`), kv(`b`, 2, `v2`)),
			expected: []string{
				`committed reverse scan non-atomic timestamps: ` +
					`[rs]{a-c}:{0:[0.000000002,0, <max>), gap:[<min>, 0.000000001,0)}->["b":v2]`,
			},
		},
		{
			name: "one scan after writes returning results in wrong order",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`b`, `v2`), 2)),
				step(withScanResult(scan(`a`, `c`), scanKV(`b`, `v2`), scanKV(`a`, `v1`))),
			},
			kvs: kvs(kv(`a`, 1, `v1`), kv(`b`, 2, `v2`)),
			expected: []string{
				`scan result not ordered correctly: ` +
					`[s]{a-c}:{0:[0.000000002,0, <max>), 1:[0.000000001,0, <max>), gap:[<min>, <max>)}->["b":v2, "a":v1]`,
			},
		},
		{
			name: "one reverse scan after writes returning results in wrong order",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`b`, `v2`), 2)),
				step(withScanResult(reverseScan(`a`, `c`), scanKV(`a`, `v1`), scanKV(`b`, `v2`))),
			},
			kvs: kvs(kv(`a`, 1, `v1`), kv(`b`, 2, `v2`)),
			expected: []string{
				`scan result not ordered correctly: ` +
					`[rs]{a-c}:{0:[0.000000001,0, <max>), 1:[0.000000002,0, <max>), gap:[<min>, <max>)}->["a":v1, "b":v2]`,
			},
		},
		{
			name: "one scan after writes returning results outside scan boundary",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`b`, `v2`), 2)),
				step(withResultTS(put(`c`, `v3`), 3)),
				step(withScanResult(scan(`a`, `c`), scanKV(`a`, `v1`), scanKV(`b`, `v2`), scanKV(`c`, `v3`))),
			},
			kvs: kvs(kv(`a`, 1, `v1`), kv(`b`, 2, `v2`), kv(`c`, 3, `v3`)),
			expected: []string{
				`key "c" outside scan bounds: ` +
					`[s]{a-c}:{0:[0.000000001,0, <max>), 1:[0.000000002,0, <max>), 2:[0.000000003,0, <max>), gap:[<min>, <max>)}->["a":v1, "b":v2, "c":v3]`,
			},
		},
		{
			name: "one reverse scan after writes returning results outside scan boundary",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`b`, `v2`), 2)),
				step(withResultTS(put(`c`, `v3`), 3)),
				step(withScanResult(reverseScan(`a`, `c`), scanKV(`c`, `v3`), scanKV(`b`, `v2`), scanKV(`a`, `v1`))),
			},
			kvs: kvs(kv(`a`, 1, `v1`), kv(`b`, 2, `v2`), kv(`c`, 3, `v3`)),
			expected: []string{
				`key "c" outside scan bounds: ` +
					`[rs]{a-c}:{0:[0.000000003,0, <max>), 1:[0.000000002,0, <max>), 2:[0.000000001,0, <max>), gap:[<min>, <max>)}->["c":v3, "b":v2, "a":v1]`,
			},
		},
		{
			name: "one scan in between writes",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withScanResult(scan(`a`, `c`), scanKV(`a`, `v1`))),
				step(withResultTS(put(`a`, `v2`), 2)),
			},
			kvs:      kvs(kv(`a`, 1, `v1`), kv(`a`, 2, `v2`)),
			expected: nil,
		},
		{
			name: "batch of scans after writes",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`b`, `v2`), 2)),
				step(withResult(batch(
					withScanResult(scan(`a`, `c`), scanKV(`a`, `v1`), scanKV(`b`, `v2`)),
					withScanResult(scan(`b`, `d`), scanKV(`b`, `v2`)),
					withScanResult(scan(`c`, `e`)),
				))),
			},
			kvs:      kvs(kv(`a`, 1, `v1`), kv(`b`, 2, `v2`)),
			expected: nil,
		},
		{
			name: "batch of scans after writes returning wrong values",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`b`, `v2`), 2)),
				step(withResult(batch(
					withScanResult(scan(`a`, `c`)),
					withScanResult(scan(`b`, `d`), scanKV(`b`, `v1`)),
					withScanResult(scan(`c`, `e`), scanKV(`c`, `v2`)),
				))),
			},
			kvs: kvs(kv(`a`, 1, `v1`), kv(`b`, 2, `v2`)),
			expected: []string{
				`committed batch non-atomic timestamps: ` +
					`[s]{a-c}:{gap:[<min>, 0.000000001,0)}->[] ` +
					`[s]{b-d}:{0:[0,0, 0,0), gap:[<min>, <max>)}->["b":v1] ` +
					`[s]{c-e}:{0:[0,0, 0,0), gap:[<min>, <max>)}->["c":v2]`,
			},
		},
		{
			name: "batch of scans after writes with non-empty time overlap",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`b`, `v2`), 2)),
				step(withResult(batch(
					withScanResult(scan(`a`, `c`), scanKV(`b`, `v1`)),
					withScanResult(scan(`b`, `d`), scanKV(`b`, `v1`)),
					withScanResult(scan(`c`, `e`)),
				))),
			},
			kvs: kvs(kv(`a`, 1, `v1`), kv(`b`, 2, `v2`)),
			expected: []string{
				`committed batch non-atomic timestamps: ` +
					`[s]{a-c}:{0:[0,0, 0,0), gap:[<min>, 0.000000001,0)}->["b":v1] ` +
					`[s]{b-d}:{0:[0,0, 0,0), gap:[<min>, <max>)}->["b":v1] ` +
					`[s]{c-e}:{gap:[<min>, <max>)}->[]`,
			},
		},
		{
			name: "transactional scans with non-empty time overlap",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`a`, `v2`), 3)),
				step(withResultTS(put(`b`, `v3`), 2)),
				step(withResultTS(put(`b`, `v4`), 3)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withScanResult(scan(`a`, `c`), scanKV(`a`, `v1`), scanKV(`b`, `v3`)),
					withScanResult(scan(`b`, `d`), scanKV(`b`, `v3`)),
				), 2)),
			},
			// Reading v1 is valid from 1-3 and v3 is valid from 2-3: overlap 2-3
			kvs:      kvs(kv(`a`, 1, `v1`), kv(`a`, 3, `v2`), kv(`b`, 2, `v3`), kv(`b`, 3, `v4`)),
			expected: nil,
		},
		{
			name: "transactional scans after delete with non-empty time overlap",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`a`, `v2`), 3)),
				step(withResultTS(put(`b`, `v3`), 1)),
				step(withResultTS(del(`b`), 2)),
				step(withResultTS(put(`b`, `v4`), 4)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withScanResult(scan(`a`, `c`), scanKV(`a`, `v1`)),
					withScanResult(scan(`b`, `d`)),
				), 2)),
			},
			// Reading v1 is valid from 1-3 and <nil> for `b` is valid <min>-1 and 2-4: overlap 2-3
			kvs:      kvs(kv(`a`, 1, `v1`), kv(`a`, 3, `v2`), kv(`b`, 1, `v3`), tombstone(`b`, 2), kv(`b`, 4, `v4`)),
			expected: nil,
		},
		{
			name: "transactional scans with empty time overlap",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`a`, `v2`), 2)),
				step(withResultTS(put(`b`, `v3`), 2)),
				step(withResultTS(put(`b`, `v4`), 3)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withScanResult(scan(`a`, `c`), scanKV(`a`, `v1`), scanKV(`b`, `v3`)),
					withScanResult(scan(`b`, `d`), scanKV(`b`, `v3`)),
				), 2)),
			},
			// Reading v1 is valid from 1-2 and v3 is valid from 2-3: no overlap
			kvs: kvs(kv(`a`, 1, `v1`), kv(`a`, 2, `v2`), kv(`b`, 2, `v3`), kv(`b`, 3, `v4`)),
			expected: []string{
				`committed txn non-atomic timestamps: ` +
					`[s]{a-c}:{0:[0.000000001,0, 0.000000002,0), 1:[0.000000002,0, 0.000000003,0), gap:[<min>, <max>)}->["a":v1, "b":v3] ` +
					`[s]{b-d}:{0:[0.000000002,0, 0.000000003,0), gap:[<min>, <max>)}->["b":v3]`,
			},
		},
		{
			name: "transactional scans after delete with empty time overlap",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`a`, `v2`), 2)),
				step(withResultTS(put(`b`, `v3`), 1)),
				step(withResultTS(del(`b`), 3)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withScanResult(scan(`a`, `c`), scanKV(`a`, `v1`)),
					withScanResult(scan(`b`, `d`)),
				), 3)),
			},
			// Reading v1 is valid from 1-2 and <nil> for `b` is valid from <min>-1, 3-<max>: no overlap
			kvs: kvs(kv(`a`, 1, `v1`), kv(`a`, 2, `v2`), kv(`b`, 1, `v3`), tombstone(`b`, 3)),
			expected: []string{
				`committed txn non-atomic timestamps: ` +
					`[s]{a-c}:{0:[0.000000001,0, 0.000000002,0), gap:[<min>, 0.000000001,0),[0.000000003,0, <max>)}->["a":v1] ` +
					`[s]{b-d}:{gap:[<min>, 0.000000001,0),[0.000000003,0, <max>)}->[]`,
			},
		},
		{
			name: "transactional scans one missing with non-empty time overlap",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`a`, `v2`), 2)),
				step(withResultTS(put(`b`, `v3`), 2)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withScanResult(scan(`a`, `c`), scanKV(`a`, `v1`)),
					withScanResult(scan(`b`, `d`)),
				), 2)),
			},
			// Reading v1 is valid from 1-2 and v3 is valid from 0-2: overlap 1-2
			kvs:      kvs(kv(`a`, 1, `v1`), kv(`a`, 2, `v2`), kv(`b`, 2, `v3`)),
			expected: nil,
		},
		{
			name: "transactional scans one missing with empty time overlap",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`a`, `v2`), 2)),
				step(withResultTS(put(`b`, `v3`), 1)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withScanResult(scan(`a`, `c`), scanKV(`a`, `v1`)),
					withScanResult(scan(`b`, `d`)),
				), 1)),
			},
			// Reading v1 is valid from 1-2 and v3 is valid from 0-1: no overlap
			kvs: kvs(kv(`a`, 1, `v1`), kv(`a`, 2, `v2`), kv(`b`, 1, `v3`)),
			expected: []string{
				`committed txn non-atomic timestamps: ` +
					`[s]{a-c}:{0:[0.000000001,0, 0.000000002,0), gap:[<min>, 0.000000001,0)}->["a":v1] ` +
					`[s]{b-d}:{gap:[<min>, 0.000000001,0)}->[]`,
			},
		},
		{
			name: "transactional scan and write with non-empty time overlap",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`a`, `v2`), 3)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withScanResult(scan(`a`, `c`), scanKV(`a`, `v1`)),
					withResult(put(`b`, `v3`)),
				), 2)),
			},
			// Reading v1 is valid from 1-3 and v3 is valid at 2: overlap @2
			kvs:      kvs(kv(`a`, 1, `v1`), kv(`a`, 3, `v2`), kv(`b`, 2, `v3`)),
			expected: nil,
		},
		{
			name: "transactional scan and write with empty time overlap",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`a`, `v2`), 2)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withScanResult(scan(`a`, `c`), scanKV(`a`, `v1`)),
					withResult(put(`b`, `v3`)),
				), 2)),
			},
			// Reading v1 is valid from 1-2 and v3 is valid at 2: no overlap
			kvs: kvs(kv(`a`, 1, `v1`), kv(`a`, 2, `v2`), kv(`b`, 2, `v3`)),
			expected: []string{
				`committed txn non-atomic timestamps: ` +
					`[s]{a-c}:{0:[0.000000001,0, 0.000000002,0), gap:[<min>, <max>)}->["a":v1] [w]"b":0.000000002,0->v3`,
			},
		},
		{
			name: "transaction with scan before and after write",
			steps: []Step{
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withScanResult(scan(`a`, `c`)),
					withResult(put(`a`, `v1`)),
					withScanResult(scan(`a`, `c`), scanKV(`a`, `v1`)),
				), 1)),
			},
			kvs:      kvs(kv(`a`, 1, `v1`)),
			expected: nil,
		},
		{
			name: "transaction with incorrect scan before write",
			steps: []Step{
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withScanResult(scan(`a`, `c`), scanKV(`a`, `v1`)),
					withResult(put(`a`, `v1`)),
					withScanResult(scan(`a`, `c`), scanKV(`a`, `v1`)),
				), 1)),
			},
			kvs: kvs(kv(`a`, 1, `v1`)),
			expected: []string{
				`committed txn non-atomic timestamps: ` +
					`[s]{a-c}:{0:[0,0, 0,0), gap:[<min>, <max>)}->["a":v1] ` +
					`[w]"a":0.000000001,0->v1 ` +
					`[s]{a-c}:{0:[0.000000001,0, <max>), gap:[<min>, <max>)}->["a":v1]`,
			},
		},
		{
			name: "transaction with incorrect scan after write",
			steps: []Step{
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withScanResult(scan(`a`, `c`)),
					withResult(put(`a`, `v1`)),
					withScanResult(scan(`a`, `c`)),
				), 1)),
			},
			kvs: kvs(kv(`a`, 1, `v1`)),
			expected: []string{
				`committed txn non-atomic timestamps: ` +
					`[s]{a-c}:{gap:[<min>, <max>)}->[] [w]"a":0.000000001,0->v1 [s]{a-c}:{gap:[<min>, 0.000000001,0)}->[]`,
			},
		},
		{
			name: "two transactionally committed puts of the same key with scans",
			steps: []Step{
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withScanResult(scan(`a`, `c`)),
					withResult(put(`a`, `v1`)),
					withScanResult(scan(`a`, `c`), scanKV(`a`, `v1`)),
					withResult(put(`a`, `v2`)),
					withScanResult(scan(`a`, `c`), scanKV(`a`, `v2`)),
					withResult(put(`b`, `v3`)),
					withScanResult(scan(`a`, `c`), scanKV(`a`, `v2`), scanKV(`b`, `v3`)),
				), 1)),
			},
			kvs:      kvs(kv(`a`, 1, `v2`), kv(`b`, 1, `v3`)),
			expected: nil,
		},
		{
			name: "one deleterange before write",
			steps: []Step{
				step(withDeleteRangeResult(delRange(`a`, `c`))),
				step(withResult(put(`a`, `v1`))),
			},
			kvs:      kvs(kv(`a`, 1, `v1`)),
			expected: nil,
		},
		{
			name: "one deleterange before write returning wrong value",
			steps: []Step{
				step(withDeleteRangeResult(delRange(`a`, `c`), roachpb.Key(`a`))),
				step(withResult(put(`a`, `v1`))),
			},
			kvs: kvs(kv(`a`, 1, `v1`)),
			expected: []string{
				`committed deleteRange missing write: ` +
					`[dr.s]{a-c}:{0:[0.000000001,0, <max>), gap:[<min>, <max>)}->["a"] ` +
					`[dr.d]"a":missing-><nil>`,
			},
		},
		{
			name: "one deleterange after write",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withDeleteRangeResult(delRange(`a`, `c`), roachpb.Key(`a`)),
				), 2)),
			},
			kvs:      kvs(kv(`a`, 1, `v1`), tombstone(`a`, 2)),
			expected: nil,
		},
		{
			name: "one deleterange after write returning wrong value",
			steps: []Step{
				step(withResult(put(`a`, `v1`))),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withDeleteRangeResult(delRange(`a`, `c`)),
				), 2)),
			},
			kvs: kvs(kv(`a`, 1, `v1`), tombstone(`a`, 2)),
			expected: []string{
				`extra writes: [d]"a":uncertain-><nil>`,
			},
		},
		{
			name: "one deleterange after write missing write",
			steps: []Step{
				step(withResult(put(`a`, `v1`))),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withDeleteRangeResult(delRange(`a`, `c`), roachpb.Key(`a`)),
				), 1)),
			},
			kvs: kvs(kv(`a`, 1, `v1`)),
			expected: []string{
				`committed txn missing write: ` +
					`[dr.s]{a-c}:{0:[0.000000001,0, <max>), gap:[<min>, <max>)}->["a"] ` +
					`[dr.d]"a":missing-><nil>`,
			},
		},
		{
			name: "one deleterange after writes",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`b`, `v2`), 2)),
				step(withResultTS(put(`c`, `v3`), 3)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withDeleteRangeResult(delRange(`a`, `c`), roachpb.Key(`a`), roachpb.Key(`b`)),
				), 4)),
				step(withScanResult(scan(`a`, `d`), scanKV(`c`, `v3`))),
			},
			kvs:      kvs(kv(`a`, 1, `v1`), kv(`b`, 2, `v2`), kv(`c`, 3, `v3`), tombstone(`a`, 4), tombstone(`b`, 4)),
			expected: nil,
		},
		{
			name: "one deleterange after writes with write timestamp disagreement",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`b`, `v2`), 2)),
				step(withResultTS(put(`c`, `v3`), 3)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withDeleteRangeResult(delRange(`a`, `c`), roachpb.Key(`a`), roachpb.Key(`b`)),
				), 4)),
				step(withScanResult(scan(`a`, `d`), scanKV(`c`, `v3`))),
			},
			kvs: kvs(kv(`a`, 1, `v1`), kv(`b`, 2, `v2`), kv(`c`, 3, `v3`), tombstone(`a`, 4), tombstone(`b`, 5)),
			expected: []string{
				`committed txn missing write: ` +
					`[dr.s]{a-c}:{0:[0.000000001,0, <max>), 1:[0.000000002,0, 0.000000005,0), gap:[<min>, <max>)}->["a", "b"] ` +
					`[dr.d]"a":0.000000004,0-><nil> [dr.d]"b":missing-><nil>`,
			},
		},
		{
			name: "one deleterange after writes with missing write",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`b`, `v2`), 2)),
				step(withResultTS(put(`c`, `v3`), 3)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withDeleteRangeResult(delRange(`a`, `c`), roachpb.Key(`a`), roachpb.Key(`b`)),
				), 4)),
				step(withScanResult(scan(`a`, `d`), scanKV(`c`, `v3`))),
			},
			kvs: kvs(kv(`a`, 1, `v1`), kv(`b`, 2, `v2`), kv(`c`, 3, `v3`), tombstone(`a`, 4)),
			expected: []string{
				`committed txn missing write: ` +
					`[dr.s]{a-c}:{0:[0.000000001,0, <max>), 1:[0.000000002,0, <max>), gap:[<min>, <max>)}->["a", "b"] ` +
					`[dr.d]"a":0.000000004,0-><nil> [dr.d]"b":missing-><nil>`,
				`committed scan non-atomic timestamps: [s]{a-d}:{0:[0.000000003,0, <max>), gap:[<min>, 0.000000001,0)}->["c":v3]`,
			},
		},
		{
			name: "one deleterange after writes and delete",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`b`, `v2`), 2)),
				step(withResultTS(del(`a`), 4)),
				step(withResultTS(put(`a`, `v3`), 5)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withDeleteRangeResult(delRange(`a`, `c`), roachpb.Key(`a`), roachpb.Key(`b`)),
				), 3)),
			},
			kvs:      kvs(kv(`a`, 1, `v1`), kv(`b`, 2, `v2`), tombstone(`a`, 3), tombstone(`b`, 3), tombstone(`a`, 4), kv(`a`, 5, `v3`)),
			expected: nil,
		},
		{
			name: "one transactional deleterange followed by put after writes",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withDeleteRangeResult(delRange(`a`, `c`), roachpb.Key(`a`)),
					withResult(put(`b`, `v2`)),
				), 2)),
			},
			kvs:      kvs(kv(`a`, 1, `v1`), tombstone(`a`, 2), kv(`b`, 2, `v2`)),
			expected: nil,
		},
		{
			name: "one transactional deleterange followed by put after writes with write timestamp disagreement",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withDeleteRangeResult(delRange(`a`, `c`), roachpb.Key(`a`)),
					withResult(put(`b`, `v2`)),
				), 2)),
			},
			kvs: kvs(kv(`a`, 1, `v1`), tombstone(`a`, 2), kv(`b`, 3, `v2`)),
			expected: []string{
				`committed txn non-atomic timestamps: ` +
					`[dr.s]{a-c}:{0:[0.000000001,0, <max>), gap:[<min>, <max>)}->["a"] ` +
					`[dr.d]"a":0.000000002,0-><nil> [w]"b":0.000000003,0->v2`,
			},
		},
		{
			name: "one transactional put shadowed by deleterange after writes",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withResult(put(`b`, `v2`)),
					withDeleteRangeResult(delRange(`a`, `c`), roachpb.Key(`a`), roachpb.Key(`b`)),
				), 2)),
			},
			kvs:      kvs(kv(`a`, 1, `v1`), tombstone(`a`, 2), tombstone(`b`, 2)),
			expected: nil,
		},
		{
			name: "one transactional put shadowed by deleterange after writes with write timestamp disagreement",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withResult(put(`b`, `v2`)),
					withDeleteRangeResult(delRange(`a`, `c`), roachpb.Key(`a`), roachpb.Key(`b`)),
				), 2)),
			},
			kvs: kvs(kv(`a`, 1, `v1`), tombstone(`a`, 2), tombstone(`b`, 3)),
			expected: []string{
				`committed txn missing write: ` +
					`[w]"b":missing->v2 ` +
					`[dr.s]{a-c}:{0:[0.000000001,0, <max>), 1:[0,0, <max>), gap:[<min>, <max>)}->["a", "b"] ` +
					`[dr.d]"a":0.000000002,0-><nil> [dr.d]"b":missing-><nil>`,
			},
		},
		{
			name: "one deleterange after writes returning keys outside span boundary",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`d`, `v2`), 2)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withDeleteRangeResult(delRange(`a`, `c`), roachpb.Key(`a`), roachpb.Key(`d`)),
				), 3)),
			},
			kvs: kvs(kv(`a`, 1, `v1`), tombstone(`a`, 3), kv(`d`, 2, `v2`)),
			expected: []string{
				`key "d" outside delete range bounds: ` +
					`[dr.s]{a-c}:{0:[0.000000001,0, <max>), 1:[0.000000002,0, <max>), gap:[<min>, <max>)}->["a", "d"] ` +
					`[dr.d]"a":0.000000003,0-><nil> [dr.d]"d":missing-><nil>`,
			},
		},
		{
			name: "one deleterange after writes incorrectly deleting keys outside span boundary",
			steps: []Step{
				step(withResultTS(put(`a`, `v1`), 1)),
				step(withResultTS(put(`d`, `v2`), 2)),
				step(withResultTS(closureTxn(ClosureTxnType_Commit,
					withDeleteRangeResult(delRange(`a`, `c`), roachpb.Key(`a`), roachpb.Key(`d`)),
				), 3)),
			},
			kvs: kvs(kv(`a`, 1, `v1`), tombstone(`a`, 3), kv(`d`, 2, `v2`), tombstone(`d`, 3)),
			expected: []string{
				`key "d" outside delete range bounds: ` +
					`[dr.s]{a-c}:{0:[0.000000001,0, <max>), 1:[0.000000002,0, <max>), gap:[<min>, <max>)}->["a", "d"] ` +
					`[dr.d]"a":0.000000003,0-><nil> [dr.d]"d":0.000000003,0-><nil>`,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e, err := MakeEngine()
			require.NoError(t, err)
			defer e.Close()
			for _, kv := range test.kvs {
				e.Put(kv.Key, kv.Value)
			}
			var actual []string
			if failures := Validate(test.steps, e); len(failures) > 0 {
				actual = make([]string, len(failures))
				for i := range failures {
					actual[i] = failures[i].Error()
				}
			}
			assert.Equal(t, test.expected, actual)
		})
	}
}
