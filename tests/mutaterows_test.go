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
	"log"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/googleapis/cloud-bigtable-clients-test/testproxypb"
	"github.com/stretchr/testify/assert"
	btpb "google.golang.org/genproto/googleapis/bigtable/v2"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/durationpb"
)

// buildEntryData returns an instance of entryData type based on the mutated and failed rows.
// Row indices are used: all the rows with `mutatedRowIndices` are mutated successfully;
// and all the rows with `failedRowIndices` have failures with `errorCode`.
// The function will check if non-OK `errorCode` is passed in when there are failed rows.
func buildEntryData(mutatedRowIndices []int, failedRowIndices []int, errorCode codes.Code) entryData {
	result := entryData{}
	if len(mutatedRowIndices) > 0 {
		result.mutatedRows = mutatedRowIndices
	}
	if len(failedRowIndices) > 0 {
		if errorCode == codes.OK {
			log.Fatal("errorCode should be non-OK")
		}
		result.failedRows = map[codes.Code][]int{errorCode: failedRowIndices}
	}
	return result
}

// dummyMutateRowsRequestCore returns a dummy MutateRowsRequest for the given table and rowkeys.
// For simplicity, only one "SetCell" mutation is used for each row; family & column names, values,
// and timestamps are hard-coded.
func dummyMutateRowsRequestCore(tableID string, rowKeys []string) *btpb.MutateRowsRequest {
	req := &btpb.MutateRowsRequest{
		TableName: buildTableName(tableID),
		Entries:   []*btpb.MutateRowsRequest_Entry{},
	}
	for i := 0; i < len(rowKeys); i++ {
		entry := &btpb.MutateRowsRequest_Entry{
			RowKey: []byte(rowKeys[i]),
			Mutations: []*btpb.Mutation{
				{Mutation: &btpb.Mutation_SetCell_{
					SetCell: &btpb.Mutation_SetCell{
						FamilyName:      "f",
						ColumnQualifier: []byte("col"),
						TimestampMicros: 1000,
						Value:           []byte("value"),
					},
				}},
			},
		}
		req.Entries = append(req.Entries, entry)
	}
	return req
}

// dummyMutateRowsRequest returns a dummy MutateRowsRequest for the given table and row count.
// rowkeys and values are generated with the row indices.
func dummyMutateRowsRequest(tableID string, numRows int) *btpb.MutateRowsRequest {
	rowKeys := []string{}
	for i := 0; i < numRows; i++ {
		rowKeys = append(rowKeys, "row-"+strconv.Itoa(i))
	}
	return dummyMutateRowsRequestCore(tableID, rowKeys)
}

// TestMutateRows_Generic_Headers tests that MutateRows request has client and resource info, as
// well as app_profile_id in the header.
func TestMutateRows_Generic_Headers(t *testing.T) {
	// 0. Common variables
	const numRows int = 2
	const profileID string = "test_profile"
	const tableID string = "table"
	tableName := buildTableName(tableID)

	// 1. Instantiate the mock server
	// Don't call mockMutateRowsFn() as the behavior is to record metadata of the request
	mdRecords := make(chan metadata.MD, 1)
	server := initMockServer(t)
	server.MutateRowsFn = func(req *btpb.MutateRowsRequest, srv btpb.Bigtable_MutateRowsServer) error {
		md, _ := metadata.FromIncomingContext(srv.Context())
		mdRecords <- md

		// C++ client requires per-row result to be set, otherwise the client returns Internal error.
		// For Java client, using "return nil" is enough.
		res := &btpb.MutateRowsResponse{}
		for i := 0; i < numRows; i++ {
			res.Entries = append(res.Entries, &btpb.MutateRowsResponse_Entry{
				Index:  int64(i),
				Status: &status.Status{},
			})
		}
		return srv.Send(res)
	}

	// 2. Build the request to test proxy
	req := testproxypb.MutateRowsRequest{
		ClientId: t.Name(),
		Request:  dummyMutateRowsRequest(tableID, numRows),
	}

	// 3. Perform the operation via test proxy
	opts := clientOpts{
		profile: profileID,
	}
	doMutateRowsOp(t, server, &req, &opts)

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

// TestMutateRows_NoRetry_NonTransientErrors tests that client will not retry on non-transient errors.
func TestMutateRows_NoRetry_NonTransientErrors(t *testing.T) {
	// 0. Common variables
	const numRows int = 4
	const numRPCs int = 1
	const tableID string = "table"
	mutatedRowIndices := []int{0, 3}
	failedRowIndices := []int{1, 2}

	// 1. Instantiate the mock server
	recorder := make(chan *mutateRowsReqRecord, numRPCs+1)
	action := &mutateRowsAction{ // There are 4 rows to mutate, row-1 and row-2 have errors.
		data: buildEntryData(mutatedRowIndices, failedRowIndices, codes.PermissionDenied),
	}
	server := initMockServer(t)
	server.MutateRowsFn = mockMutateRowsFnSimple(recorder, action)

	// 2. Build the request to test proxy
	req := testproxypb.MutateRowsRequest{
		ClientId: t.Name(),
		Request:  dummyMutateRowsRequest(tableID, numRows),
	}

	// 3. Perform the operation via test proxy
	res := doMutateRowsOp(t, server, &req, nil)

	// 4a. Check the number of requests in the recorder
	assert.Equal(t, numRPCs, len(recorder))

	// 4b. Check the two failed rows
	assert.Equal(t, 2, len(res.GetEntries()))
	outputIndices := []int{}
	for _, entry := range res.GetEntries() {
		outputIndices = append(outputIndices, int(entry.GetIndex()))
		assert.Equal(t, int32(codes.PermissionDenied), entry.GetStatus().GetCode())
	}
	assert.ElementsMatch(t, failedRowIndices, outputIndices)
}

// TestMutateRows_Generic_DeadlineExceeded tests that client-side timeout is set and respected.
func TestMutateRows_Generic_DeadlineExceeded(t *testing.T) {
	// 0. Common variables
	const numRows int = 1
	const numRPCs int = 1
	const tableID string = "table"

	// 1. Instantiate the mock server
	recorder := make(chan *mutateRowsReqRecord, numRPCs+1)
	action := &mutateRowsAction{ // There is one row to mutate, which has a long delay.
		data:     buildEntryData([]int{0}, nil, 0),
		delayStr: "10s",
	}
	server := initMockServer(t)
	server.MutateRowsFn = mockMutateRowsFnSimple(recorder, action)

	// 2. Build the request to test proxy
	req := testproxypb.MutateRowsRequest{
		ClientId: t.Name(),
		Request:  dummyMutateRowsRequest(tableID, numRows),
	}

	// 3. Perform the operation via test proxy
	opts := clientOpts{
		timeout: &durationpb.Duration{Seconds: 2},
	}
	res := doMutateRowsOp(t, server, &req, &opts)
	curTs := time.Now()

	// 4a. Check the number of requests in the recorder
	assert.Equal(t, numRPCs, len(recorder))

	// 4b. Check the runtime
	loggedReq := <-recorder
	runTimeSecs := int(curTs.Unix() - loggedReq.ts.Unix())
	assert.GreaterOrEqual(t, runTimeSecs, 2)
	assert.Less(t, runTimeSecs, 8) // 8s (< 10s of server delay time) indicates timeout takes effect.

	// 4c. Check the failed row
	assert.Equal(t, int32(codes.DeadlineExceeded), res.GetStatus().GetCode())
	if len(res.GetEntries()) != 0 {
		assert.Equal(t, 1, len(res.GetEntries()))
		for _, entry := range res.GetEntries() {
			assert.Equal(t, int32(codes.DeadlineExceeded), entry.GetStatus().GetCode())
		}
	}
}

// TestMutateRows_Retry_TransientErrors tests that client will retry transient errors.
func TestMutateRows_Retry_TransientErrors(t *testing.T) {
	// 0. Common variables
	const numRows int = 4
	const numRPCs int = 3
	const tableID string = "table"
	clientReq := dummyMutateRowsRequest(tableID, numRows)

	// 1. Instantiate the mock server
	recorder := make(chan *mutateRowsReqRecord, numRPCs+1)
	actions := []*mutateRowsAction{
		&mutateRowsAction{ // There are 4 rows to mutate, row-1 and row-2 have errors.
			data:        buildEntryData([]int{0, 3}, []int{1, 2}, codes.Unavailable),
			endOfStream: true,
		},
		&mutateRowsAction{ // Retry for the two failed rows, row-1 has error.
			data:        buildEntryData([]int{1}, []int{0}, codes.Unavailable),
			endOfStream: true,
		},
		&mutateRowsAction{ // Retry for the one failed row, which has no error.
			data: buildEntryData([]int{0}, nil, 0),
		},
	}
	server := initMockServer(t)
	server.MutateRowsFn = mockMutateRowsFn(recorder, actions)

	// 2. Build the request to test proxy
	req := testproxypb.MutateRowsRequest{
		ClientId: t.Name(),
		Request:  clientReq,
	}

	// 3. Perform the operation via test proxy
	res := doMutateRowsOp(t, server, &req, nil)

	// 4a. Check that the overall operation succeeded
	checkResultOkStatus(t, res)

	// 4b. Check the number of requests in the recorder
	assert.Equal(t, numRPCs, len(recorder))

	// 4c. Check the recorded requests
	origReq := <-recorder
	firstRetry := <-recorder
	secondRetry := <-recorder

	if diff := cmp.Diff(clientReq, origReq.req, protocmp.Transform()); diff != "" {
		t.Errorf("diff found (-want +got):\n%s", diff)
	}

	expectedFirstRetry := dummyMutateRowsRequestCore(tableID, []string{"row-1", "row-2"})
	if diff := cmp.Diff(expectedFirstRetry, firstRetry.req, protocmp.Transform()); diff != "" {
		t.Errorf("diff found (-want +got):\n%s", diff)
	}

	expectedSecondRetry := dummyMutateRowsRequestCore(tableID, []string{"row-1"})
	if diff := cmp.Diff(expectedSecondRetry, secondRetry.req, protocmp.Transform()); diff != "" {
		t.Errorf("diff found (-want +got):\n%s", diff)
	}
}

// TestMutateRows_Retry_ExponentialBackoff tests that client will retry using exponential backoff.
// TODO: as the clients use jitter with different defaults, a correct and reliable check should look
// at more retry attempts. Before finding the best solution, we drop the check for now.
func TestMutateRows_Retry_ExponentialBackoff(t *testing.T) {
	// 0. Common variables
	const numRows int = 1
	const numRPCs int = 4
	const tableID string = "table"

	// 1. Instantiate the mock server
	recorder := make(chan *mutateRowsReqRecord, numRPCs+1)
	actions := []*mutateRowsAction{
		&mutateRowsAction{ // There is one row to mutate, which has error.
			data:        buildEntryData(nil, []int{0}, codes.Unavailable),
			endOfStream: true,
		},
		&mutateRowsAction{ // There is one row to mutate, which has error.
			data:        buildEntryData(nil, []int{0}, codes.Unavailable),
			endOfStream: true,
		},
		&mutateRowsAction{ // There is one row to mutate, which has error.
			data:        buildEntryData(nil, []int{0}, codes.Unavailable),
			endOfStream: true,
		},
		&mutateRowsAction{ // There is one row to mutate, which has no error.
			data: buildEntryData([]int{0}, nil, 0),
		},
	}
	server := initMockServer(t)
	server.MutateRowsFn = mockMutateRowsFn(recorder, actions)

	// 2. Build the request to test proxy
	req := testproxypb.MutateRowsRequest{
		ClientId: t.Name(),
		Request:  dummyMutateRowsRequest(tableID, numRows),
	}

	// 3. Perform the operation via test proxy
	doMutateRowsOp(t, server, &req, nil)

	// 4a. Check the number of requests in the recorder
	assert.Equal(t, numRPCs, len(recorder))

	// 4b. Log the retry delays
	origReq := <-recorder

	for n := 1; n < numRPCs; n += 1 {
		select {
		case retry := <-recorder:
			delay := int(retry.ts.UnixMilli() - origReq.ts.UnixMilli())
			// Different clients may have different behaviors, we log the delays for informational purpose.
			// Example: For the first retry delay, C++ client uses 100ms but Java client uses 10ms.
			t.Logf("Retry #%d delay: %dms", n, delay)
		case <-time.After(500 * time.Millisecond):
			t.Logf("Retry #%d: Timeout waiting for retry (expecting %d retries)", n, numRPCs-1)
		}
	}
}

// TestMutateRows_Generic_MultiStreams tests that client can have multiple concurrent streams.
func TestMutateRows_Generic_MultiStreams(t *testing.T) {
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
	recorder := make(chan *mutateRowsReqRecord, requestRecorderCapacity)
	actions := make([]*mutateRowsAction, concurrency)
	for i := 0; i < concurrency; i++ {
		// Each request will succeed in the batch mutations.
		actions[i] = &mutateRowsAction{
			data:     buildEntryData([]int{0, 1}, nil, 0),
			delayStr: "2s",
		}
	}
	server := initMockServer(t)
	server.MutateRowsFn = mockMutateRowsFnSimple(recorder, actions...)

	// 2. Build the requests to test proxy
	reqs := make([]*testproxypb.MutateRowsRequest, concurrency)
	for i := 0; i < concurrency; i++ {
		clientReq := dummyMutateRowsRequestCore("table", rowKeys[i])
		reqs[i] = &testproxypb.MutateRowsRequest{
			ClientId: t.Name(),
			Request:  clientReq,
		}
	}

	// 3. Perform the operations via test proxy
	results := doMutateRowsOps(t, server, reqs, nil)

	// 4a. Check that all the requests succeeded
	assert.Equal(t, concurrency, len(results))
	checkResultOkStatus(t, results...)

	// 4b. Check that the timestamps of requests should be very close
	assert.Equal(t, concurrency, len(recorder))
	checkRequestsAreWithin(t, 1000, recorder)
}

// TestMutateRows_Generic_CloseClient tests that client doesn't kill inflight requests after
// client closing, but will reject new requests.
func TestMutateRows_Generic_CloseClient(t *testing.T) {
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
	recorder := make(chan *mutateRowsReqRecord, requestRecorderCapacity)
	actions := make([]*mutateRowsAction, 2*halfBatchSize)
	for i := 0; i < 2*halfBatchSize; i++ {
		actions[i] = &mutateRowsAction{
			data:     buildEntryData([]int{0, 1}, nil, 0),
			delayStr: "2s",
		}
	}
	server := initMockServer(t)
	server.MutateRowsFn = mockMutateRowsFnSimple(recorder, actions...)

	// 2. Build the requests to test proxy
	reqsBatchOne := make([]*testproxypb.MutateRowsRequest, halfBatchSize) // Will be finished
	reqsBatchTwo := make([]*testproxypb.MutateRowsRequest, halfBatchSize) // Will be rejected by client
	for i := 0; i < halfBatchSize; i++ {
		reqsBatchOne[i] = &testproxypb.MutateRowsRequest{
			ClientId: clientID,
			Request:  dummyMutateRowsRequestCore("table", rowKeys[i]),
		}
		reqsBatchTwo[i] = &testproxypb.MutateRowsRequest{
			ClientId: clientID,
			Request:  dummyMutateRowsRequestCore("table", rowKeys[i+halfBatchSize]),
		}
	}

	// 3. Perform the operations via test proxy
	setUp(t, server, clientID, nil)
	defer tearDown(t, server, clientID)

	closeClientAfter := time.Second
	resultsBatchOne := doMutateRowsOpsCore(t, clientID, reqsBatchOne, &closeClientAfter)
	resultsBatchTwo := doMutateRowsOpsCore(t, clientID, reqsBatchTwo, nil)

	// 4a. Check that server only receives batch-one requests
	assert.Equal(t, halfBatchSize, len(recorder))

	// 4b. Check that all the batch-one requests succeeded or were cancelled
	checkResultOkOrCancelledStatus(t, resultsBatchOne...)

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

// TestMutateRows_Retry_WithRoutingCookie tests that client handles routing cookie correctly.
func TestMutateRows_Retry_WithRoutingCookie(t *testing.T) {
	// 0. Common variables
	const tableID string = "table"
	cookie := "test-cookie"
	clientReq := dummyMutateRowsRequest(tableID, 1)

	// 1. Instantiate the mock server
	recorder := make(chan *mutateRowsReqRecord, 2)
	mdRecorder := make(chan metadata.MD, 2)
	actions := []*mutateRowsAction{
		&mutateRowsAction{rpcError: codes.Unavailable, routingCookie: cookie},
		&mutateRowsAction{data: buildEntryData([]int{0}, nil, 0)},
	}
	server := initMockServer(t)
	server.MutateRowsFn = mockMutateRowsFnWithMetadata(recorder, mdRecorder, actions)

	// 2. Build the request to test proxy
	req := testproxypb.MutateRowsRequest{
		ClientId: t.Name(),
		Request:  clientReq,
	}

	// 3. Perform the operation via test proxy
	res := doMutateRowsOp(t, server, &req, nil)

	// 4a. Check that the overall operation succeeded
	checkResultOkStatus(t, res)

	// 4b. Verify routing cookie is seen
	// Ignore the first metadata which won't have the routing cookie
	var _ = <-mdRecorder

	select {
	case md1 := <-mdRecorder:
		// second metadata which comes from the retry attempt should have a routing cookie field
		val := md1["x-goog-cbt-cookie-test"]
		assert.NotEmpty(t, val)
		if len(val) == 0 {
			return
		}
		assert.Equal(t, cookie, val[0])
	case <-time.After(100 * time.Millisecond):
		t.Error("Timeout waiting for requests on recorder channel")
	}
}

// TestMutateRows_Retry_WithRetryInfo tests that client is handling RetryInfo correctly.
func TestMutateRows_Retry_WithRetryInfo(t *testing.T) {
	// 0. Common variable
	const tableID string = "table"
	clientReq := dummyMutateRowsRequest(tableID, 1)

	// 1. Instantiate the mock server
	recorder := make(chan *mutateRowsReqRecord, 2)
	mdRecorder := make(chan metadata.MD, 2)
	actions := []*mutateRowsAction{
		&mutateRowsAction{rpcError: codes.Unavailable, retryInfo: "2s"},
		&mutateRowsAction{data: buildEntryData([]int{0}, nil, 0)},
	}
	server := initMockServer(t)
	server.MutateRowsFn = mockMutateRowsFnWithMetadata(recorder, mdRecorder, actions)

	// 2. Build the request to test proxy
	req := testproxypb.MutateRowsRequest{
		ClientId: t.Name(),
		Request:  clientReq,
	}

	// 3. Perform the operation via test proxy
	res := doMutateRowsOp(t, server, &req, nil)

	// 4a. Check that the overall operation succeeded
	checkResultOkStatus(t, res)

	// 4b. Verify retry backoff time is correct
	firstReq := <-recorder
	retryReq := <-recorder
	firstReqTs := firstReq.ts.Unix()
	retryReqTs := retryReq.ts.Unix()

	assert.True(t, retryReqTs-firstReqTs >= 2)
}
