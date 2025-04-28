// Copyright 2022 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !emulator
// +build !emulator

package tests

import (
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	btpb "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	"github.com/google/go-cmp/cmp"
	"github.com/googleapis/cloud-bigtable-clients-test/testproxypb"
	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/durationpb"
)

// dummyChunkData returns a chunkData object with hardcoded family name and qualifier.
func dummyChunkData(rowKey string, value string, status RowStatus) chunkData {
	return chunkData{
		rowKey: []byte(rowKey), familyName: "f", qualifier: "col", value: value, status: status}
}

// TestReadRows_Generic_Headers tests that ReadRows request has client and resource info, as well as
// app_profile_id in the header.
func TestReadRows_Generic_Headers(t *testing.T) {
	// 0. Common variables
	const profileID string = "test_profile"
	tableName := buildTableName("table")

	// 1. Instantiate the mock server
	// Don't call mockReadRowsFn() as the behavior is to record metadata of the request
	mdRecords := make(chan metadata.MD, 1)
	server := initMockServer(t)
	server.ReadRowsFn = func(req *btpb.ReadRowsRequest, srv btpb.Bigtable_ReadRowsServer) error {
		md, _ := metadata.FromIncomingContext(srv.Context())
		mdRecords <- md
		return nil
	}

	// 2. Build the request to test proxy
	req := testproxypb.ReadRowsRequest{
		ClientId: t.Name(),
		Request:  &btpb.ReadRowsRequest{TableName: tableName},
	}

	// 3. Perform the operation via test proxy
	opts := clientOpts{
		profile: profileID,
	}
	doReadRowsOp(t, server, &req, &opts)

	// 4. Check the request headers in the metadata
	md := <-mdRecords
	if len(md["user-agent"]) == 0 && len(md["x-goog-api-client"]) == 0 {
		assert.Fail(t, "Client info is missing in the request header")
	}

	resource := md["x-goog-request-params"][0]
	if !strings.Contains(resource, tableName) && !strings.Contains(resource, url.QueryEscape(tableName)) {
		assert.Fail(t, "Resource info is missing in the request header")
	}
	assert.Contains(t, resource, profileID)
}

// TestReadRows_NoRetry_OutOfOrderError tests that client will fail on receiving out of order row keys.
func TestReadRows_NoRetry_OutOfOrderError(t *testing.T) {
	// 1. Instantiate the mock server
	action := &readRowsAction{
		chunks: []chunkData{
			dummyChunkData("row-01", "v1", Commit),
			// The following two rows are in bad order
			dummyChunkData("row-07", "v7", Commit),
			dummyChunkData("row-03", "v3", Commit),
		},
	}
	server := initMockServer(t)
	server.ReadRowsFn = mockReadRowsFnSimple(nil, action)

	// 2. Build the request to test proxy
	req := testproxypb.ReadRowsRequest{
		ClientId: t.Name(),
		Request:  &btpb.ReadRowsRequest{TableName: buildTableName("table")},
	}

	// 3. Perform the operation via test proxy
	res := doReadRowsOp(t, server, &req, nil)

	// 4. Check the response (C++ and Java clients have different error messages)
	assert.Contains(t, res.GetStatus().GetMessage(), "increasing")
	t.Logf("The full error message is: %s", res.GetStatus().GetMessage())
}

func TestReadRows_ReverseScans_FeatureFlag_Enabled(t *testing.T) {
	// 1. Instantiate the mock server
	// Don't call mockReadRowsFn() as the behavior is to record metadata of the request
	mdRecords := make(chan metadata.MD, 1)
	server := initMockServer(t)
	server.ReadRowsFn = func(req *btpb.ReadRowsRequest, srv btpb.Bigtable_ReadRowsServer) error {
		md, _ := metadata.FromIncomingContext(srv.Context())
		mdRecords <- md
		return nil
	}

	// 2. Build the request to test proxy
	req := testproxypb.ReadRowsRequest{
		ClientId: t.Name(),
		Request:  &btpb.ReadRowsRequest{TableName: buildTableName("table"), Reversed: true},
	}

	// 3. Perform the operation via test proxy
	doReadRowsOp(t, server, &req, nil)

	// 4. Check the request headers in the metadata
	md := <-mdRecords

	ff, err := getClientFeatureFlags(md)
	assert.Nil(t, err, "failed to decode client feature flags")
	assert.True(t, ff != nil && ff.ReverseScans, "client must enable ReverseScans feature flag")
}

// TestReadRows_NoRetry_OutOfOrderError_Reverse tests that client will fail on receiving out of order row keys for reverse scans.
func TestReadRows_NoRetry_OutOfOrderError_Reverse(t *testing.T) {
	// 1. Instantiate the mock server
	action := &readRowsAction{
		chunks: []chunkData{
			dummyChunkData("row-03", "v3", Commit),
			// The following two rows are in bad order
			dummyChunkData("row-07", "v7", Commit),
			dummyChunkData("row-01", "v1", Commit),
		},
	}
	server := initMockServer(t)
	server.ReadRowsFn = mockReadRowsFnSimple(nil, action)

	// 2. Build the request to test proxyk
	req := testproxypb.ReadRowsRequest{
		ClientId: t.Name(),
		Request:  &btpb.ReadRowsRequest{TableName: buildTableName("table"), Reversed: true},
	}

	// 3. Perform the operation via test proxy
	res := doReadRowsOp(t, server, &req, nil)

	// 4. Check the response (C++ and Java clients have different error messages)
	assert.Contains(t, res.GetStatus().GetMessage(), "decreasing")
	t.Logf("The full error message is: %s", res.GetStatus().GetMessage())
}

// TestReadRows_NoRetry_ErrorAfterLastRow tests that when receiving a transient error after receiving
// the last row, the read will still finish successfully.
func TestReadRows_NoRetry_ErrorAfterLastRow(t *testing.T) {
	// 1. Instantiate the mock server
	sequence := []*readRowsAction{
		&readRowsAction{
			chunks: []chunkData{
				dummyChunkData("row-01", "v1", Commit)}},
		&readRowsAction{rpcError: codes.DeadlineExceeded}, // Error after returning the requested row
		&readRowsAction{
			chunks: []chunkData{
				dummyChunkData("row-05", "v5", Commit)}},
	}
	server := initMockServer(t)
	server.ReadRowsFn = mockReadRowsFn(nil, sequence)

	// 2. Build the request to test proxy
	req := testproxypb.ReadRowsRequest{
		ClientId: t.Name(),
		Request: &btpb.ReadRowsRequest{
			TableName: buildTableName("table"),
			RowsLimit: 1,
		},
	}

	// 3. Perform the operation via test proxy
	res := doReadRowsOp(t, server, &req, nil)

	// 4a. Verify that the read succeeds
	checkResultOkStatus(t, res)
	assert.Equal(t, 1, len(res.GetRows()))
	assert.Equal(t, "row-01", string(res.Rows[0].Key))
}

// TestReadRows_Retry_PausedScan tests that client will transparently resume the scan when a stream
// is paused.
func TestReadRows_Retry_PausedScan(t *testing.T) {
	// 0. Common variables
	clientReq := &btpb.ReadRowsRequest{TableName: buildTableName("table")}

	// 1. Instantiate the mock server
	recorder := make(chan *readRowsReqRecord, 2)
	sequence := []*readRowsAction{
		&readRowsAction{
			chunks: []chunkData{
				dummyChunkData("row-01", "v1", Commit)}},
		&readRowsAction{rpcError: codes.Aborted}, // close the stream by aborting it
		&readRowsAction{
			chunks: []chunkData{
				dummyChunkData("row-05", "v5", Commit)}},
	}
	server := initMockServer(t)
	server.ReadRowsFn = mockReadRowsFn(recorder, sequence)

	// 2. Build the request to test proxy
	req := testproxypb.ReadRowsRequest{
		ClientId: t.Name(),
		Request:  clientReq,
	}

	// 3. Perform the operation via test proxy
	res := doReadRowsOp(t, server, &req, nil)

	// 4a. Verify that two rows were read successfully
	checkResultOkStatus(t, res)
	assert.Equal(t, 2, len(res.GetRows()))
	assert.Equal(t, "row-01", string(res.Rows[0].Key))
	assert.Equal(t, "row-05", string(res.Rows[1].Key))

	// 4b. Verify that client sent the requests properly
	origReq := <-recorder
	retryReq := <-recorder
	if diff := cmp.Diff(clientReq, origReq.req, protocmp.Transform(), protocmp.IgnoreEmptyMessages()); diff != "" {
		origRows := origReq.req.GetRows()
		// Check if rows or row ranges are present in requests. This is a workaround for the NodeJS client.
		// In Node we add an empty row range to a full table scan request to simplify the resumption logic.
		if origRows == nil || origRows.GetRowRanges() == nil {
			// If rows don't exist in either request, skip the comparison
			t.Logf("Skipping rows comparison: As this is a full table scan")
		} else if len(origRows.GetRowRanges()) == 1 && origRows.GetRowRanges()[0].GetStartKey() == nil && origRows.GetRowRanges()[0].GetEndKey() == nil {
			// If rows don't exist in either request, skip the comparison
			t.Logf("Skipping rows comparison: As this is a full table scan")
		} else {
			// Otherwise, proceed with the comparison and report any differences
			t.Errorf("diff found (-want +got):\n%s", diff)
		}
	}
	assert.True(t, cmp.Equal(retryReq.req.GetRows().GetRowRanges()[0].StartKey, &btpb.RowRange_StartKeyOpen{StartKeyOpen: []byte("row-01")}))
}

// TestReadRows_Retry_LastScannedRow tests that client will resume from last scan row key.
func TestReadRows_Retry_LastScannedRow(t *testing.T) {
	// 1. Instantiate the mock server
	recorder := make(chan *readRowsReqRecord, 2)
	sequence := []*readRowsAction{
		&readRowsAction{
			chunks: []chunkData{
				dummyChunkData("abar", "v_a", Commit)}},
		&readRowsAction{
			chunks: []chunkData{
				dummyChunkData("qfoo", "v_q", Drop)}}, // Chunkless response due to Drop
		&readRowsAction{rpcError: codes.Unavailable}, // A retry-able error
		&readRowsAction{
			chunks: []chunkData{
				dummyChunkData("zbar", "v_z", Commit)}},
	}
	server := initMockServer(t)
	server.ReadRowsFn = mockReadRowsFn(recorder, sequence)

	// 2. Build the request to test proxy
	req := testproxypb.ReadRowsRequest{
		ClientId: t.Name(),
		Request:  &btpb.ReadRowsRequest{TableName: buildTableName("table")},
	}

	// 3. Perform the operation via test proxy
	res := doReadRowsOp(t, server, &req, nil)

	// 4a. Verify that rows aabar and zzbar were read successfully (qqfoo doesn't match the filter)
	checkResultOkStatus(t, res)
	assert.Equal(t, 2, len(res.GetRows()))
	assert.Equal(t, "abar", string(res.Rows[0].Key))
	assert.Equal(t, "zbar", string(res.Rows[1].Key))

	// 4b. Verify that client sent the retry request properly
	loggedReq := <-recorder
	loggedRetry := <-recorder
	if len(loggedReq.req.GetRows().GetRowRanges()) > 0 {
		// Some clients such as Node.js may add an empty row range to the row range list.
		assert.Equal(t, 1, len(loggedReq.req.GetRows().GetRowRanges()))
		assert.Empty(t, loggedReq.req.GetRows().GetRowRanges()[0])
	}
	assert.True(t, cmp.Equal(loggedRetry.req.GetRows().GetRowRanges()[0].StartKey, &btpb.RowRange_StartKeyOpen{StartKeyOpen: []byte("qfoo")}))
}

// TestReadRows_Retry_LastScannedRow_Reverse tests that client will resume from last scan row key when reverse scanning.
func TestReadRows_Retry_LastScannedRow_Reverse(t *testing.T) {
	// 1. Instantiate the mock server
	recorder := make(chan *readRowsReqRecord, 2)
	sequence := []*readRowsAction{
		&readRowsAction{
			chunks: []chunkData{
				dummyChunkData("zbar", "v_z", Commit)}},
		&readRowsAction{
			chunks: []chunkData{
				dummyChunkData("qfoo", "v_q", Drop)}}, // Chunkless response due to Drop
		&readRowsAction{rpcError: codes.Unavailable}, // A retry-able error
		&readRowsAction{
			chunks: []chunkData{
				dummyChunkData("abar", "v_a", Commit)}},
	}
	server := initMockServer(t)
	server.ReadRowsFn = mockReadRowsFn(recorder, sequence)

	// 2. Build the request to test proxy
	req := testproxypb.ReadRowsRequest{
		ClientId: t.Name(),
		Request:  &btpb.ReadRowsRequest{TableName: buildTableName("table"), Reversed: true},
	}

	// 3. Perform the operation via test proxy
	res := doReadRowsOp(t, server, &req, nil)

	// 4a. Verify that rows zbar and abar were read successfully (qfoo doesn't match the filter)
	checkResultOkStatus(t, res)
	assert.Equal(t, 2, len(res.GetRows()))
	assert.Equal(t, "zbar", string(res.Rows[0].Key))
	assert.Equal(t, "abar", string(res.Rows[1].Key))

	// 4b. Verify that client sent the retry request properly
	loggedReq := <-recorder
	loggedRetry := <-recorder
	if len(loggedReq.req.GetRows().GetRowRanges()) > 0 {
		// Some clients such as Node.js may add an empty row range to the row range list.
		assert.Equal(t, 1, len(loggedReq.req.GetRows().GetRowRanges()))
		assert.Empty(t, loggedReq.req.GetRows().GetRowRanges()[0])
	}
	assert.True(t, cmp.Equal(loggedRetry.req.GetRows().GetRowRanges()[0].EndKey, &btpb.RowRange_EndKeyOpen{EndKeyOpen: []byte("qfoo")}))
}

// TestReadRows_Generic_MultiStreams tests that client can have multiple concurrent streams.
func TestReadRows_Generic_MultiStreams(t *testing.T) {
	// 0. Common variable
	rowKeys := [][]string{
		[]string{"op0-row-a", "op0-row-b"},
		[]string{"op1-row-a", "op1-row-b"},
		[]string{"op2-row-a", "op2-row-b"},
		[]string{"op3-row-a", "op3-row-b"},
		[]string{"op4-row-a", "op4-row-b"},
	}
	concurrency := len(rowKeys)
	const requestRecorderCapacity = 10

	// 1. Instantiate the mock server
	recorder := make(chan *readRowsReqRecord, requestRecorderCapacity)
	actions := make([]*readRowsAction, concurrency)
	for i := 0; i < concurrency; i++ {
		// Each request will get a different response.
		actions[i] = &readRowsAction{
			chunks: []chunkData{
				dummyChunkData(rowKeys[i][0], fmt.Sprintf("value%d-a", i), Commit),
				dummyChunkData(rowKeys[i][1], fmt.Sprintf("value%d-b", i), Commit),
			},
			delayStr: "2s",
		}
	}
	server := initMockServer(t)
	server.ReadRowsFn = mockReadRowsFnSimple(recorder, actions...)

	// 2. Build the requests to test proxy
	reqs := make([]*testproxypb.ReadRowsRequest, concurrency)
	for i := 0; i < concurrency; i++ {
		reqs[i] = &testproxypb.ReadRowsRequest{
			ClientId: t.Name(),
			Request: &btpb.ReadRowsRequest{
				TableName: buildTableName("table"),
				Rows: &btpb.RowSet{
					RowKeys: [][]byte{[]byte(rowKeys[i][0]), []byte(rowKeys[i][1])},
				},
			},
		}
	}

	// 3. Perform the operations via test proxy
	results := doReadRowsOps(t, server, reqs, nil)

	// 4a. Check that all the requests succeeded
	assert.Equal(t, concurrency, len(results))
	checkResultOkStatus(t, results...)

	// 4b. Check that the timestamps of requests should be very close
	assert.Equal(t, concurrency, len(recorder))
	checkRequestsAreWithin(t, 1000, recorder)

	// 4c. Check the row keys in the results.
	for i := 0; i < concurrency; i++ {
		assert.Equal(t, rowKeys[i][0], string(results[i].Rows[0].Key))
		assert.Equal(t, rowKeys[i][1], string(results[i].Rows[1].Key))
	}
}

// TestReadRows_Retry_StreamReset tests that client will retry on stream reset.
func TestReadRows_Retry_StreamReset(t *testing.T) {
	// 0. Common variable
	const maxConnAge = 4 * time.Second
	const maxConnAgeGrace = time.Second

	// 1. Instantiate the mock server
	recorder := make(chan *readRowsReqRecord, 3)
	sequence := []*readRowsAction{
		&readRowsAction{
			chunks: []chunkData{
				dummyChunkData("abar", "v_a", Commit)}},
		&readRowsAction{
			chunks: []chunkData{
				dummyChunkData("qbar", "v_q", Commit)},
			delayStr: "10s"}, // Stream resets before sending chunks.
		&readRowsAction{
			chunks: []chunkData{
				dummyChunkData("qbar", "v_q", Commit)}},
		&readRowsAction{
			chunks: []chunkData{
				dummyChunkData("zbar", "v_z", Commit)}},
	}
	serverOpt := grpc.KeepaliveParams(
		keepalive.ServerParameters{
			MaxConnectionAge:      maxConnAge,
			MaxConnectionAgeGrace: maxConnAgeGrace,
		})
	server := initMockServer(t, serverOpt)
	server.ReadRowsFn = mockReadRowsFn(recorder, sequence)

	// 2. Build the request to test proxy
	req := testproxypb.ReadRowsRequest{
		ClientId: t.Name(),
		Request:  &btpb.ReadRowsRequest{TableName: buildTableName("table")},
	}

	// 3. Perform the operation via test proxy
	res := doReadRowsOp(t, server, &req, nil)

	// 4a. Verify that rows were read successfully
	checkResultOkStatus(t, res)
	assert.Equal(t, 3, len(res.GetRows()))
	assert.Equal(t, "abar", string(res.Rows[0].Key))
	assert.Equal(t, "qbar", string(res.Rows[1].Key))
	assert.Equal(t, "zbar", string(res.Rows[2].Key))

	// 4b. Verify that client sent the only retry request properly
	assert.Equal(t, 2, len(recorder))
	loggedReq := <-recorder
	loggedRetry := <-recorder
	if reflect.TypeOf(loggedReq.req.GetRows().GetRowRanges()).Kind() == reflect.Slice {
		// Check if rows or row ranges are present in requests. This is a workaround for the NodeJS client.
		// In Node we add an empty row range to a full table scan request to simplify the resumption logic.
		assert.Empty(t, 0, len(loggedReq.req.GetRows().GetRowRanges()))
	} else {
		assert.Empty(t, loggedReq.req.GetRows().GetRowRanges())
	}
	assert.True(t, cmp.Equal(loggedRetry.req.GetRows().GetRowRanges()[0].StartKey, &btpb.RowRange_StartKeyOpen{StartKeyOpen: []byte("abar")}))
}

// TestReadRows_NoRetry_MultipleIndividualRowKeys tests that the client can request multiple
// individual row keys to scan
func TestReadRows_NoRetry_MultipleIndividualRowKeys(t *testing.T) {
	k1 := "abar"
	k2 := "qbar"
	k3 := "zbar"

	// 1. Instantiate the mock server
	rec := make(chan *readRowsReqRecord, 3)
	seq := []*readRowsAction{
		&readRowsAction{
			chunks: []chunkData{
				dummyChunkData(k1, "v_a", Commit)}},
		&readRowsAction{
			chunks: []chunkData{
				dummyChunkData(k2, "v_q", Commit)}},
		&readRowsAction{
			chunks: []chunkData{
				dummyChunkData(k3, "v_z", Commit)}},
	}
	server := initMockServer(t)
	server.ReadRowsFn = mockReadRowsFn(rec, seq)

	// 2. Build the request to test proxy
	req := testproxypb.ReadRowsRequest{
		ClientId: t.Name(),
		Request: &btpb.ReadRowsRequest{
			TableName: buildTableName("table"),
			Rows: &btpb.RowSet{
				RowKeys: [][]byte{
					[]byte(k1),
					[]byte(k2),
					[]byte(k3),
				},
			},
		},
	}

	// 3. Perform the operation via test proxy
	res := doReadRowsOp(t, server, &req, nil)
	assert.Len(t, res.Rows, 3)

}

// TestReadRows_NoRetry_EmptyTableNoRows tests that reads on an empty table returns 0 rows.
func TestReadRows_NoRetry_EmptyTableNoRows(t *testing.T) {
	// 1. Instantiate the mock server
	recorder := make(chan *readRowsReqRecord, 3)
	action := &readRowsAction{
		chunks: []chunkData{}}
	server := initMockServer(t)
	server.ReadRowsFn = mockReadRowsFnSimple(recorder, action)

	// 2. Build the request to test proxy
	req := testproxypb.ReadRowsRequest{
		ClientId: t.Name(),
		Request:  &btpb.ReadRowsRequest{TableName: buildTableName("table")},
	}

	// 3. Perform the operation via test proxy
	res := doReadRowsOp(t, server, &req, nil)
	assert.Len(t, res.Rows, 0)
}

// TestReadRows_NoRetry_MultipleRowRanges tests that the client can request multiple
// row ranges to scan
func TestReadRows_NoRetry_MultipleRowRanges(t *testing.T) {
	k1 := "abar"
	k2 := "kbar"
	k3 := "qbar"
	k4 := "zbar"

	// 1. Instantiate the mock server
	rec := make(chan *readRowsReqRecord, 3)
	seq := []*readRowsAction{
		{
			chunks: []chunkData{dummyChunkData(k1, "v_a", Commit)},
		},
		{
			chunks: []chunkData{dummyChunkData(k2, "v_k", Commit)},
		},
		{
			chunks: []chunkData{dummyChunkData(k3, "v_q", Commit)},
		},
		{
			chunks: []chunkData{dummyChunkData(k4, "v_z", Commit)},
		},
	}
	server := initMockServer(t)
	server.ReadRowsFn = mockReadRowsFn(rec, seq)

	// 2. Build the request to test proxy
	req := testproxypb.ReadRowsRequest{
		ClientId: t.Name(),
		Request: &btpb.ReadRowsRequest{
			TableName: buildTableName("table"),
			Rows: &btpb.RowSet{
				RowRanges: []*btpb.RowRange{
					{
						StartKey: &btpb.RowRange_StartKeyClosed{
							StartKeyClosed: []byte(k1),
						},
						EndKey: &btpb.RowRange_EndKeyClosed{
							EndKeyClosed: []byte(k2),
						},
					},
					{
						StartKey: &btpb.RowRange_StartKeyClosed{
							StartKeyClosed: []byte(k3),
						},
						EndKey: &btpb.RowRange_EndKeyClosed{
							EndKeyClosed: []byte(k4),
						},
					},
				},
			},
		},
	}

	// 3. Perform the operation via test proxy
	res := doReadRowsOp(t, server, &req, nil)
	assert.Len(t, res.Rows, 4)
}

// TestReadRows_NoRetry_ClosedStartUnspecifiedEnd tests that the client can request
// a row range with a closed start key and no end key.
func TestReadRows_NoRetry_ClosedStartUnspecifiedEnd(t *testing.T) {
	keys := []string{"abar", "kbar"}
	cfs := []string{"v_a", "v_k"}

	rec := make(chan *readRowsReqRecord, 3)
	seq := []*readRowsAction{
		{
			chunks: []chunkData{dummyChunkData(keys[0], cfs[0], Commit)},
		},
		{
			chunks: []chunkData{dummyChunkData(keys[1], cfs[1], Commit)},
		},
	}

	// 1. Instantiate the mock server
	server := initMockServer(t)
	server.ReadRowsFn = mockReadRowsFn(rec, seq)

	// 2. Build the request to test proxy
	req := testproxypb.ReadRowsRequest{
		ClientId: t.Name(),
		Request: &btpb.ReadRowsRequest{
			TableName: buildTableName("table"),
			Rows: &btpb.RowSet{
				RowRanges: []*btpb.RowRange{
					{
						StartKey: &btpb.RowRange_StartKeyClosed{
							StartKeyClosed: []byte(keys[0]),
						},
					},
				},
			},
		},
	}

	// 3. Perform the operation via test proxy
	res := doReadRowsOp(t, server, &req, nil)
	assert.Len(t, res.Rows, 2)
}

// todo reverse scan version
// TestReadRows_NoRetry_OpenEndUnspecifiedStart tests that the client can request
// a row range with an open end key and no start key.
func TestReadRows_NoRetry_OpenEndUnspecifiedStart(t *testing.T) {
	keys := []string{"abar", "kbar"}
	values := []string{"v_a", "v_k"}

	rec := make(chan *readRowsReqRecord, 3)

	// 1. Instantiate the mock server
	server := initMockServer(t)
	server.ReadRowsFn = mockReadRowsFnSimple(rec, &readRowsAction{
		chunks: []chunkData{dummyChunkData(keys[0], values[0], Commit)},
	})

	// 2. Build the request to test proxy
	req := testproxypb.ReadRowsRequest{
		ClientId: t.Name(),
		Request: &btpb.ReadRowsRequest{
			TableName: buildTableName("table"),
			Rows: &btpb.RowSet{
				RowRanges: []*btpb.RowRange{
					{
						EndKey: &btpb.RowRange_EndKeyOpen{
							EndKeyOpen: []byte(keys[1]),
						},
					},
				},
			},
		},
	}

	// 3. Perform the operation via test proxy
	res := doReadRowsOp(t, server, &req, nil)
	assert.Len(t, res.Rows, 1)
}

// TestReadRows_Generic_CloseClient tests that client doesn't kill inflight requests after
// client closing, but will reject new requests.
func TestReadRows_Generic_CloseClient(t *testing.T) {
	// 0. Common variable
	rowKeys := [][]string{
		[]string{"op0-row-a", "op0-row-b"},
		[]string{"op1-row-a", "op1-row-b"},
		[]string{"op2-row-a", "op2-row-b"},
		[]string{"op3-row-a", "op3-row-b"},
		[]string{"op4-row-a", "op4-row-b"},
		[]string{"op5-row-a", "op5-row-b"},
	}
	halfBatchSize := len(rowKeys) / 2
	clientID := t.Name()
	const requestRecorderCapacity = 10

	// 1. Instantiate the mock server
	recorder := make(chan *readRowsReqRecord, requestRecorderCapacity)
	actions := make([]*readRowsAction, 2*halfBatchSize)
	for i := 0; i < 2*halfBatchSize; i++ {
		// Each request will get a different response.
		actions[i] = &readRowsAction{
			chunks: []chunkData{
				dummyChunkData(rowKeys[i][0], fmt.Sprintf("value%d-a", i), Commit),
				dummyChunkData(rowKeys[i][1], fmt.Sprintf("value%d-b", i), Commit),
			},
			delayStr: "2s",
		}
	}
	server := initMockServer(t)
	server.ReadRowsFn = mockReadRowsFnSimple(recorder, actions...)

	// 2. Build the requests to test proxy
	reqsBatchOne := make([]*testproxypb.ReadRowsRequest, halfBatchSize) // Will be finished
	reqsBatchTwo := make([]*testproxypb.ReadRowsRequest, halfBatchSize) // Will be rejected by client
	for i := 0; i < halfBatchSize; i++ {
		reqsBatchOne[i] = &testproxypb.ReadRowsRequest{
			ClientId: clientID,
			Request: &btpb.ReadRowsRequest{
				TableName: buildTableName("table"),
				Rows: &btpb.RowSet{
					RowKeys: [][]byte{[]byte(rowKeys[i][0]), []byte(rowKeys[i][1])},
				},
			},
		}
		reqsBatchTwo[i] = &testproxypb.ReadRowsRequest{
			ClientId: clientID,
			Request: &btpb.ReadRowsRequest{
				TableName: buildTableName("table"),
				Rows: &btpb.RowSet{
					RowKeys: [][]byte{
						[]byte(rowKeys[i+halfBatchSize][0]),
						[]byte(rowKeys[i+halfBatchSize][1]),
					},
				},
			},
		}
	}

	// 3. Perform the operations via test proxy
	setUp(t, server, clientID, nil)
	defer tearDown(t, server, clientID)

	closeClientAfter := time.Second
	resultsBatchOne := doReadRowsOpsCore(t, clientID, reqsBatchOne, &closeClientAfter)
	resultsBatchTwo := doReadRowsOpsCore(t, clientID, reqsBatchTwo, nil)

	// 4a. Check that server only receives batch-one requests
	assert.Equal(t, halfBatchSize, len(recorder))

	// 4b. Check that all the batch-one requests succeeded or were cancelled
	checkResultOkOrCancelledStatus(t, resultsBatchOne...)
	for i := 0; i < halfBatchSize; i++ {
		assert.NotNil(t, resultsBatchOne[i])
		if resultsBatchOne[i] == nil {
			continue
		}
		resCode := resultsBatchOne[i].GetStatus().GetCode()
		if resCode == int32(codes.Canceled) {
			continue
		}
		assert.NotNil(t, resultsBatchOne[i].Rows)
		if resultsBatchOne[i].Rows == nil {
			continue
		}
		assert.Equal(t, rowKeys[i][0], string(resultsBatchOne[i].Rows[0].Key))
		assert.Equal(t, rowKeys[i][1], string(resultsBatchOne[i].Rows[1].Key))
	}

	// 4c. Check that all the batch-two requests failed at the proxy level:
	// the proxy tries to use close client. Client and server have nothing to blame.
	// We are a little permissive here by just checking if failures occur.
	for i := 0; i < halfBatchSize; i++ {
		if resultsBatchTwo[i] == nil {
			continue
		}
		assert.NotEmpty(t, resultsBatchTwo[i].GetStatus().GetCode())
	}
}

// TestReadRows_Generic_DeadlineExceeded tests that client-side timeout is set and respected.
func TestReadRows_Generic_DeadlineExceeded(t *testing.T) {
	// 1. Instantiate the mock server
	recorder := make(chan *readRowsReqRecord, 1)
	action := &readRowsAction{
		chunks:   []chunkData{dummyChunkData("row-01", "v1", Commit)},
		delayStr: "10s",
	}
	server := initMockServer(t)
	server.ReadRowsFn = mockReadRowsFnSimple(recorder, action)

	// 2. Build the request to test proxy
	req := testproxypb.ReadRowsRequest{
		ClientId: t.Name(),
		Request:  &btpb.ReadRowsRequest{TableName: buildTableName("table")},
	}

	// 3. Perform the operation via test proxy
	opts := clientOpts{
		timeout: &durationpb.Duration{Seconds: 2},
	}
	res := doReadRowsOp(t, server, &req, &opts)

	// 4a. Check the runtime
	curTs := time.Now()
	loggedReq := <-recorder
	runTimeSecs := int(curTs.Unix() - loggedReq.ts.Unix())
	assert.GreaterOrEqual(t, runTimeSecs, 2)
	assert.Less(t, runTimeSecs, 8) // 8s (< 10s of server delay time) indicates timeout takes effect.

	// 4b. Check the DeadlineExceeded error. Some clients wrap the error code in the message,
	// so check the message if error code is not right.
	if res.GetStatus().GetCode() != int32(codes.DeadlineExceeded) {
		msg := res.GetStatus().GetMessage()
		assert.Contains(t, strings.ToLower(strings.ReplaceAll(msg, " ", "")), "deadlineexceeded")
	}
}

// TestReadRows_Retry_WithRoutingCookie tests that routing cookie is handled correctly by the client.
func TestReadRows_Retry_WithRoutingCookie(t *testing.T) {
	// 0. Common variable
	cookie := "test-cookie"

	// 1. Instantiate the mock server
	sequence := []*readRowsAction{
		&readRowsAction{
			chunks: []chunkData{
				dummyChunkData("row-01", "v1", Commit)}},
		&readRowsAction{rpcError: codes.Unavailable, routingCookie: cookie}, // Error with a routing cookie
		&readRowsAction{
			chunks: []chunkData{
				dummyChunkData("row-05", "v5", Commit)}},
	}
	server := initMockServer(t)

	mdRecords := make(chan metadata.MD, 2)
	recorder := make(chan *readRowsReqRecord, 2)
	server.ReadRowsFn = mockReadRowsFnWithMetadata(recorder, mdRecords, sequence)

	// 2. Build the request to test proxy
	req := testproxypb.ReadRowsRequest{
		ClientId: t.Name(),
		Request: &btpb.ReadRowsRequest{
			TableName: buildTableName("table"),
		},
	}

	// 3. Perform the operation via test proxy
	res := doReadRowsOp(t, server, &req, nil)

	// 4a. Verify that the read succeeds
	checkResultOkStatus(t, res)
	assert.Equal(t, 2, len(res.GetRows()))
	assert.Equal(t, "row-01", string(res.Rows[0].Key))
	assert.Equal(t, "row-05", string(res.Rows[1].Key))

	// 4b. Verify routing cookie is seen
	// Ignore the first metadata which won't have the routing cookie
	var _ = <-mdRecords
	// second metadata which comes from the retry attempt should have a routing cookie field
	md1 := <-mdRecords
	val := md1["x-goog-cbt-cookie-test"]
	assert.NotEmpty(t, val)
	if len(val) == 0 {
		return
	}
	assert.Equal(t, cookie, val[0])

	// 4c. Verify retry request is correct
	var _ = <-recorder
	retryReq := <-recorder
	assert.True(t, cmp.Equal(retryReq.req.GetRows().GetRowRanges()[0].StartKey, &btpb.RowRange_StartKeyOpen{StartKeyOpen: []byte("row-01")}))
}

// TestReadRows_Retry_WithRoutingCookie_MultipleErrorResponses tests handling of routing cookie
// in multiple error responses for the same request. Returns a cookie first, then return an error
// without a cookie, and return an error with a new cookie. The second retry should have the same
// cookie as the first retry, and the last retry should have the new cookie.
func TestReadRows_Retry_WithRoutingCookie_MultipleErrorResponses(t *testing.T) {
	// 0. Common variable
	cookie := "test-cookie"
	newCookie := "new-test-cookie"

	// 1. Instantiate the mock server
	sequence := []*readRowsAction{
		&readRowsAction{
			chunks: []chunkData{
				dummyChunkData("row-01", "v1", Commit)}},
		&readRowsAction{rpcError: codes.Unavailable, routingCookie: cookie},    // Error with a routing cookie
		&readRowsAction{rpcError: codes.Unavailable},                           // Error with no routing cookie
		&readRowsAction{rpcError: codes.Unavailable, routingCookie: newCookie}, //Error with new routing cookie
		&readRowsAction{
			chunks: []chunkData{
				dummyChunkData("row-05", "v5", Commit)}},
	}
	server := initMockServer(t)

	mdRecords := make(chan metadata.MD, 4)
	recorder := make(chan *readRowsReqRecord, 4)
	server.ReadRowsFn = mockReadRowsFnWithMetadata(recorder, mdRecords, sequence)

	// 2. Build the request to test proxy
	req := testproxypb.ReadRowsRequest{
		ClientId: t.Name(),
		Request: &btpb.ReadRowsRequest{
			TableName: buildTableName("table"),
		},
	}

	// 3. Perform the operation via test proxy
	res := doReadRowsOp(t, server, &req, nil)

	// 4a. Verify that the read succeeds
	checkResultOkStatus(t, res)
	assert.Equal(t, 2, len(res.GetRows()))
	if len(res.GetRows()) > 0 {
		assert.Equal(t, "row-01", string(res.Rows[0].Key))
	}

	if len(res.GetRows()) > 1 {
		assert.Equal(t, "row-05", string(res.Rows[1].Key))
	}

	// 4b. Verify routing cookie is seen
	// Ignore the first metadata which won't have the routing cookie
	var _ = <-mdRecords
	// second metadata which comes from the retry attempt should have a routing cookie field
	md1 := <-mdRecords
	val1 := md1["x-goog-cbt-cookie-test"]
	assert.NotEmpty(t, val1)
	if len(val1) == 0 {
		return
	}
	assert.Equal(t, cookie, val1[0])
	// third metadata which comes from the 2nd retry attempt should use the same routing cookie
	md2 := <-mdRecords
	val2 := md2["x-goog-cbt-cookie-test"]
	assert.Equal(t, cookie, val2[0])
	// 4th metadata which comes from the 3rd retry attempt should have the new routing cookie
	md3 := <-mdRecords
	val3 := md3["x-goog-cbt-cookie-test"]
	assert.Equal(t, newCookie, val3[0])

	// 4c. Verify retry request is correct
	var _ = <-recorder
	retryReq1 := <-recorder
	assert.True(t, cmp.Equal(retryReq1.req.GetRows().GetRowRanges()[0].StartKey, &btpb.RowRange_StartKeyOpen{StartKeyOpen: []byte("row-01")}))
	retryReq2 := <-recorder
	assert.True(t, cmp.Equal(retryReq2.req.GetRows().GetRowRanges()[0].StartKey, &btpb.RowRange_StartKeyOpen{StartKeyOpen: []byte("row-01")}))
	retryReq3 := <-recorder
	assert.True(t, cmp.Equal(retryReq3.req.GetRows().GetRowRanges()[0].StartKey, &btpb.RowRange_StartKeyOpen{StartKeyOpen: []byte("row-01")}))
}

// TestReadRows_Retry_WithRetryInfo tests that RetryInfo is handled correctly by the client.
func TestReadRows_Retry_WithRetryInfo(t *testing.T) {
	// 1. Instantiate the mock server
	sequence := []*readRowsAction{
		&readRowsAction{
			chunks: []chunkData{
				dummyChunkData("row-01", "v1", Commit)}},
		&readRowsAction{rpcError: codes.Unavailable, retryInfo: "2s"}, // Error with retry info
		&readRowsAction{
			chunks: []chunkData{
				dummyChunkData("row-05", "v5", Commit)}},
	}
	server := initMockServer(t)

	recorder := make(chan *readRowsReqRecord, 2)
	server.ReadRowsFn = mockReadRowsFn(recorder, sequence)

	// 2. Build the request to test proxy
	req := testproxypb.ReadRowsRequest{
		ClientId: t.Name(),
		Request: &btpb.ReadRowsRequest{
			TableName: buildTableName("table"),
		},
	}

	// 3. Perform the operation via test proxy
	res := doReadRowsOp(t, server, &req, nil)

	// 4a. Verify that the read succeeds
	checkResultOkStatus(t, res)
	assert.Equal(t, 2, len(res.GetRows()))
	assert.Equal(t, "row-01", string(res.Rows[0].Key))
	assert.Equal(t, "row-05", string(res.Rows[1].Key))

	// 4b. Verify retry request is correct
	firstReq := <-recorder
	retryReq := <-recorder
	assert.True(t, cmp.Equal(retryReq.req.GetRows().GetRowRanges()[0].StartKey, &btpb.RowRange_StartKeyOpen{StartKeyOpen: []byte("row-01")}))

	// 4c. Verify retry backoff time is correct
	firstReqTs := firstReq.ts.Unix()
	retryReqTs := retryReq.ts.Unix()

	assert.True(t, retryReqTs-firstReqTs >= 2)
}

// TestReadRows_Retry_WithRetryInfo tests that RetryInfo is handled correctly by the client.
// When server stopped sending a retry info back, client fallbacks to using the initial retry
// delay.
func TestReadRows_Retry_WithRetryInfo_MultipleErrorResponse(t *testing.T) {
	// 1. Instantiate the mock server
	sequence := []*readRowsAction{
		&readRowsAction{
			chunks: []chunkData{
				dummyChunkData("row-01", "v1", Commit)}},
		&readRowsAction{rpcError: codes.Unavailable, retryInfo: "2s"}, // Error with retry info
		&readRowsAction{rpcError: codes.Unavailable},                  // Second error without retry info
		&readRowsAction{
			chunks: []chunkData{
				dummyChunkData("row-05", "v5", Commit)}},
	}
	server := initMockServer(t)

	recorder := make(chan *readRowsReqRecord, 3)
	server.ReadRowsFn = mockReadRowsFn(recorder, sequence)

	// 2. Build the request to test proxy
	req := testproxypb.ReadRowsRequest{
		ClientId: t.Name(),
		Request: &btpb.ReadRowsRequest{
			TableName: buildTableName("table"),
		},
	}

	// 3. Perform the operation via test proxy
	res := doReadRowsOp(t, server, &req, nil)

	// 4a. Verify that the read succeeds
	checkResultOkStatus(t, res)
	assert.Equal(t, 2, len(res.GetRows()))
	assert.Equal(t, "row-01", string(res.Rows[0].Key))
	assert.Equal(t, "row-05", string(res.Rows[1].Key))

	// 4b. Verify retry request is correct
	firstReq := <-recorder
	retryReq1 := <-recorder
	retryReq2 := <-recorder
	assert.True(t, cmp.Equal(retryReq1.req.GetRows().GetRowRanges()[0].StartKey, &btpb.RowRange_StartKeyOpen{StartKeyOpen: []byte("row-01")}))
	assert.True(t, cmp.Equal(retryReq2.req.GetRows().GetRowRanges()[0].StartKey, &btpb.RowRange_StartKeyOpen{StartKeyOpen: []byte("row-01")}))

	// 4c. Verify retry backoff time is correct
	firstReqTs := firstReq.ts.UnixNano() / 1e6
	retryReq1Ts := retryReq1.ts.UnixNano() / 1e6
	retryReq2Ts := retryReq2.ts.UnixNano() / 1e6
	assert.True(t, retryReq1Ts-firstReqTs >= 2000)
	// The second attempt should have delay greater than default initial delay, which is 10ms
	assert.True(t, retryReq2Ts-retryReq1Ts > 10)
}

// TestReadRows_Retry_WithRetryInfo tests that RetryInfo is handled correctly by the client.
// The overall deadline set on the client is still respected.
func TestReadRows_Retry_WithRetryInfo_OverallDedaline(t *testing.T) {
	// 1. Instantiate the mock server
	sequence := []*readRowsAction{
		&readRowsAction{
			chunks: []chunkData{
				dummyChunkData("row-01", "v1", Commit)}},
		&readRowsAction{rpcError: codes.Unavailable, retryInfo: "2s"},
		&readRowsAction{rpcError: codes.Unavailable, retryInfo: "6s"},
		&readRowsAction{
			chunks: []chunkData{
				dummyChunkData("row-05", "v5", Commit)}},
	}
	server := initMockServer(t)

	// There should only be 2 attempts due to the effect of client side timeout
	recorder := make(chan *readRowsReqRecord, 2)
	server.ReadRowsFn = mockReadRowsFn(recorder, sequence)

	// 2. Build the request to test proxy
	req := testproxypb.ReadRowsRequest{
		ClientId: t.Name(),
		Request: &btpb.ReadRowsRequest{
			TableName: buildTableName("table"),
		},
	}

	// 3. Perform the operation via test proxy
	opts := clientOpts{
		timeout: &durationpb.Duration{Seconds: 3},
	}
	doReadRowsOp(t, server, &req, &opts)

	// 4a. Check the runtime
	curTs := time.Now()
	loggedReq := <-recorder
	runTimeSecs := int(curTs.Unix() - loggedReq.ts.Unix())
	assert.Less(t, runTimeSecs, 4) // 4s is much smaller than combined retry delay indicates timeout takes effect.
}
