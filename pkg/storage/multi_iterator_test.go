// Copyright 2017 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package storage

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/stretchr/testify/require"
)

func TestMultiIterator(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	pebble, err := Open(context.Background(), InMemory(), CacheSize(1<<20 /* 1 MiB */))
	if err != nil {
		t.Fatal(err)
	}
	defer pebble.Close()

	// Each `input` is turned into an iterator and these are passed to a new
	// MultiIterator, which is fully iterated (using either NextKey or Next) and
	// turned back into a string in the same format as `input`. This is compared
	// to expectedNextKey or expectedNext.
	tests := []struct {
		inputs          []string
		expectedNextKey string
		expectedNext    string
	}{
		{[]string{}, "", ""},

		{[]string{"a1"}, "a1", "a1"},
		{[]string{"a1b1"}, "a1b1", "a1b1"},
		{[]string{"a2a1"}, "a2", "a2a1"},
		{[]string{"a2a1b1"}, "a2b1", "a2a1b1"},

		{[]string{"a1", "a2"}, "a2", "a2a1"},
		{[]string{"a2", "a1"}, "a2", "a2a1"},
		{[]string{"a1", "b2"}, "a1b2", "a1b2"},
		{[]string{"b2", "a1"}, "a1b2", "a1b2"},
		{[]string{"a1b2", "b3"}, "a1b3", "a1b3b2"},
		{[]string{"a1c2", "b3"}, "a1b3c2", "a1b3c2"},

		{[]string{"aM", "a1"}, "aM", "aMa1"},
		{[]string{"a1", "aM"}, "aM", "aMa1"},
		{[]string{"aMa2", "a1"}, "aM", "aMa2a1"},
		{[]string{"aMa1", "a2"}, "aM", "aMa2a1"},

		{[]string{"a1", "a2X"}, "a2X", "a2Xa1"},
		{[]string{"a1", "a2X", "a3"}, "a3", "a3a2Xa1"},
		{[]string{"a1", "a2Xb2"}, "a2Xb2", "a2Xa1b2"},
		{[]string{"a1b2", "a2X"}, "a2Xb2", "a2Xa1b2"},

		{[]string{"a1", "a1"}, "a1", "a1"},
		{[]string{"a4a2a1", "a4a3a1"}, "a4", "a4a3a2a1"},
		{[]string{"a1b1", "a1b2"}, "a1b2", "a1b2b1"},
		{[]string{"a1b2", "a1b1"}, "a1b2", "a1b2b1"},
		{[]string{"a1b1", "a1b1"}, "a1b1", "a1b1"},
	}
	for _, test := range tests {
		name := fmt.Sprintf("%q", test.inputs)
		t.Run(name, func(t *testing.T) {
			var iters []SimpleMVCCIterator
			for _, input := range test.inputs {
				batch := pebble.NewBatch()
				defer batch.Close()
				populateBatch(t, batch, input)
				iter := batch.NewMVCCIterator(MVCCKeyAndIntentsIterKind, IterOptions{UpperBound: roachpb.KeyMax})
				defer iter.Close()
				iters = append(iters, iter)
			}

			subtests := []iterSubtest{
				{"NextKey", test.expectedNextKey, (SimpleMVCCIterator).NextKey},
				{"Next", test.expectedNext, (SimpleMVCCIterator).Next},
			}
			for _, subtest := range subtests {
				t.Run(subtest.name, func(t *testing.T) {
					it := MakeMultiIterator(iters)
					iterateSimpleMultiIter(t, it, subtest)
				})
			}
		})
	}
}

// populateBatch populates a pebble batch with a series of MVCC key values.
// input is a string containing key, timestamp, value tuples: first a single
// character key, then a single character timestamp walltime. If the
// character after the timestamp is an M, this entry is a "metadata" key
// (timestamp=0, sorts before any non-0 timestamp, and no value). If the
// character after the timestamp is an X, this entry is a deletion
// tombstone. Otherwise the value is the same as the timestamp.
func populateBatch(t *testing.T, batch Batch, input string) {
	for i := 0; ; {
		if i == len(input) {
			break
		}
		k := []byte{input[i]}
		ts := hlc.Timestamp{WallTime: int64(input[i+1])}
		var v MVCCValue
		if i+1 < len(input) && input[i+1] == 'M' {
			ts = hlc.Timestamp{}
		} else if i+2 < len(input) && input[i+2] == 'X' {
			i++
		} else {
			v.Value.SetString(string(input[i+1]))
		}
		i += 2
		if ts.IsEmpty() {
			vRaw, err := EncodeMVCCValue(v)
			require.NoError(t, err)
			require.NoError(t, batch.PutUnversioned(k, vRaw))
		} else {
			require.NoError(t, batch.PutMVCC(MVCCKey{Key: k, Timestamp: ts}, v))
		}
	}
}

type iterSubtest struct {
	name     string
	expected string
	fn       func(SimpleMVCCIterator)
}

// iterateSimpleMultiIter iterates through a simpleMVCCIterator for expected values,
// and assumes that populateBatch populated the keys for the iterator.
func iterateSimpleMultiIter(t *testing.T, it SimpleMVCCIterator, subtest iterSubtest) {
	var output bytes.Buffer
	for it.SeekGE(MVCCKey{Key: keys.LocalMax}); ; subtest.fn(it) {
		ok, err := it.Valid()
		require.NoError(t, err)
		if !ok {
			break
		}
		output.Write(it.UnsafeKey().Key)
		if it.UnsafeKey().Timestamp.IsEmpty() {
			output.WriteRune('M')
		} else {
			output.WriteByte(byte(it.UnsafeKey().Timestamp.WallTime))
			v, err := DecodeMVCCValue(it.UnsafeValue())
			require.NoError(t, err)
			if v.IsTombstone() {
				output.WriteRune('X')
			}
		}
	}
	require.Equal(t, subtest.expected, output.String())
}
