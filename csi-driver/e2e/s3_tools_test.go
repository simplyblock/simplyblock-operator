// Unit tests for reading mint's and warp's output, which run without a
// cluster.

package e2e

import (
	"strings"
	"testing"
)

const mintSample = `progress output that is not JSON
=== mint log.json ===
{"name":"minio-go: testFunctional","function":"RemoveBucket(bucketName)","status":"FAIL","error":"no such bucket"}
{"name":"minio-go: testFunctionalV2","function":"MakeBucket","status":"FAIL","error":"valid Date or x-amz-date header"}
{"name":"minio-go: testPutObject","function":"PutObject(bucketName, objectName, reader, size, opts)","status":"PASS"}
{"name":"aws-sdk-java-v2","function":"putBucketVersioning","status":"FAIL","error":"no versioning"}
{"name":"awscli","function":"test_make_bucket","status":"FAIL","error":"boom"}
{"name":"awscli","function":"aws s3api head-object\n","status":"FAIL","error":"StorageClass was applied incorrectly"}
{"name":"mc","function":"test_put_object_with_storage_class_error","status":"FAIL"}
{"name":"minio-go: testCopyObjectV2","function":"CopyObject","status":"FAIL","error":"valid Date or x-amz-date header"}
{"name":"minio-go: testStatObjectWithVersioning","status":"FAIL","error":"Versioning has not been configured"}
{"name":"minio-go: testStorageClassMetadataPutObject","function":"testStorageClassMetadataPutObject()","status":"FAIL"}
{"name":"minio-go: testMakeBucketRegions","status":"FAIL","error":"region 'eu-central-1' is wrong"}
{"name":"minio-go: testTrailingChecksums","status":"FAIL","error":"want \"WJHaoA==\", got \"\" checksum mismatch"}
{"name":"minio-go: testRemoveMultipleObjectsWithResult","status":"FAIL","message":"Unexpected object version ID"}
{"name":"minio-go: testPresignedPostPolicy","status":"FAIL","error":"403 Forbidden"}
{"name":"s3select","function":"testCSV","status":"NA"}
`

func TestParseMintLogReadsOnlyTheRecordsAfterTheMarker(t *testing.T) {
	records, err := parseMintLog(mintSample)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 15 {
		t.Fatalf("got %d records, want 15: %+v", len(records), records)
	}
	if records[0].suite() != "minio-go" || records[0].Status != "FAIL" {
		t.Fatalf("first record is %+v", records[0])
	}
}

func TestParseMintLogWithoutTheMarkerIsAnError(t *testing.T) {
	if _, err := parseMintLog(`{"name":"x","status":"PASS"}`); err == nil {
		t.Fatal("a log without the marker parsed")
	}
}

func TestJudgeMintReportsOnlyFailuresNotRecordedUnderIssues(t *testing.T) {
	records, err := parseMintLog(mintSample)
	if err != nil {
		t.Fatal(err)
	}
	unexpected := judgeMint(records)
	// Signature version 2 and storage classes are unsupported by design, so
	// their failures are known. The awscli storage-class failure is told apart
	// from other head-object failures by its error alone.
	if len(unexpected) != 1 || !strings.HasPrefix(unexpected[0], "awscli test_make_bucket") {
		t.Fatalf("unexpected failures %v, want only awscli test_make_bucket", unexpected)
	}
}

func TestWarpErrorsMatchesNonzeroCountsOnly(t *testing.T) {
	for out, want := range map[string]bool{
		"Report: PUT. Concurrency: 16.\n * Errors: 3\n": true,
		"\x1b[91mErrors: 12\n":                          true,
		"Report: GET. Concurrency: 16.\n":               false,
		"Errors: 0\n":                                   false,
	} {
		if got := warpErrors.MatchString(out); got != want {
			t.Errorf("warpErrors.MatchString(%q) = %v, want %v", out, got, want)
		}
	}
}

func TestFailureLinesKeepsTheLinesThatNameTheError(t *testing.T) {
	if got := failureLines("short", 10); got != "short" {
		t.Errorf("failureLines(short) = %q", got)
	}
	transcript := "Removed `a` successfully.\nmc: <ERROR> Unable to remove `b`.\nRemoved `c` successfully."
	if got := failureLines(transcript, 40); got != "mc: <ERROR> Unable to remove `b`." {
		t.Errorf("failureLines kept %q, want the error line", got)
	}
	if got := failureLines("removed a, removed b, bucket not empty", 16); got != "...bucket not empty" {
		t.Errorf("failureLines without an error line kept %q, want the end", got)
	}
}
