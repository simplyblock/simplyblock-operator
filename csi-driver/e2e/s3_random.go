// S2: a randomized S3 workload checked against an in-memory model.
//
// Each worker owns two accounts, a userplus owner and a user-role outsider,
// and the buckets its owner creates. It draws operations from a seeded
// generator and checks every answer against its model: every read is compared
// byte for byte, every listing against the model's keys, every bucket's
// S3Bucket must be Ready while it exists and gone after its deletion, and the
// outsider and the other workers' owners must be refused. Workers share no
// buckets, so each model is changed by one goroutine only.
//
// S3_E2E_SEED fixes the seed, S3_E2E_DURATION the run time (default 10m), and
// S3_E2E_WORKERS the worker count (default 4). With the same seed each worker
// draws the same operation sequence, but the workers' interleaving differs
// between runs.

package e2e

import (
	"crypto/rand"
	"fmt"
	mathrand "math/rand/v2"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	ginkgo "github.com/onsi/ginkgo/v2"
	"k8s.io/kubernetes/test/e2e/framework"
)

const (
	maxBucketsPerWorker = 3
	maxObjectsPerBucket = 30
	opLogTail           = 40
)

type s3RandomConfig struct {
	seed     uint64
	duration time.Duration
	workers  int
}

func s3RandomConfigFromEnv() (s3RandomConfig, error) {
	cfg := s3RandomConfig{seed: mathrand.Uint64(), duration: 10 * time.Minute, workers: 4}
	if v := os.Getenv("S3_E2E_SEED"); v != "" {
		seed, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return cfg, fmt.Errorf("S3_E2E_SEED=%q: %w", v, err)
		}
		cfg.seed = seed
	}
	if v := os.Getenv("S3_E2E_DURATION"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("S3_E2E_DURATION=%q: %w", v, err)
		}
		cfg.duration = d
	}
	if v := os.Getenv("S3_E2E_WORKERS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return cfg, fmt.Errorf("S3_E2E_WORKERS=%q is not a positive number", v)
		}
		cfg.workers = n
	}
	return cfg, nil
}

// opRecord is one operation a worker performed and how it ended.
type opRecord struct {
	at     time.Time
	worker int
	op     string
	result string
}

// opLog keeps the last opLogTail operations of every worker for the failure
// report.
type opLog struct {
	mu      sync.Mutex
	entries map[int][]opRecord
}

func (l *opLog) add(r opRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entries := append(l.entries[r.worker], r)
	if len(entries) > opLogTail {
		entries = entries[len(entries)-opLogTail:]
	}
	l.entries[r.worker] = entries
}

func (l *opLog) tail() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var all []opRecord
	for _, entries := range l.entries {
		all = append(all, entries...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	var b strings.Builder
	for _, r := range all {
		fmt.Fprintf(&b, "%s w%d %s: %s\n", r.at.Format("15:04:05.000"), r.worker, r.op, r.result)
	}
	return b.String()
}

// s3Worker is one worker's accounts, generator, and model.
type s3Worker struct {
	id      int
	env     *s3Env
	rng     *mathrand.Rand
	log     *opLog
	owner   s3Account
	outside s3Account
	asOwner *s3.Client
	asOut   *s3.Client

	// buckets is the model: bucket name to key to content.
	buckets map[string]map[string][]byte
	created int

	// foreign is every worker's buckets, for probing another owner's.
	foreign *sync.Map
	ops     int
}

func runS3Randomized(env *s3Env) {
	cfg, err := s3RandomConfigFromEnv()
	framework.ExpectNoError(err)
	ginkgo.By(fmt.Sprintf("S3_E2E_SEED=%d S3_E2E_DURATION=%s S3_E2E_WORKERS=%d",
		cfg.seed, cfg.duration, cfg.workers))

	log := &opLog{entries: map[int][]opRecord{}}
	foreign := &sync.Map{}
	workers := make([]*s3Worker, 0, cfg.workers)
	for i := range cfg.workers {
		w := &s3Worker{
			id:      i,
			env:     env,
			rng:     mathrand.New(mathrand.NewPCG(cfg.seed, uint64(i))),
			log:     log,
			owner:   s3Account{access: fmt.Sprintf("%s-w%d-owner", env.prefix, i), secret: randomHex(20)},
			outside: s3Account{access: fmt.Sprintf("%s-w%d-out", env.prefix, i), secret: randomHex(20)},
			buckets: map[string]map[string][]byte{},
			foreign: foreign,
		}
		framework.ExpectNoError(env.gw.createUser(env.root, w.owner, roleUserPlus))
		env.users[w.owner.access] = true
		framework.ExpectNoError(env.gw.createUser(env.root, w.outside, roleUser))
		env.users[w.outside.access] = true
		w.asOwner, w.asOut = env.gw.s3Client(w.owner), env.gw.s3Client(w.outside)
		workers = append(workers, w)
	}

	deadline := time.Now().Add(cfg.duration)
	var stop atomic.Bool
	errs := make([]error, len(workers))
	var wg sync.WaitGroup
	for i, w := range workers {
		wg.Add(1)
		go func() {
			defer ginkgo.GinkgoRecover()
			defer wg.Done()
			for time.Now().Before(deadline) && !stop.Load() {
				if err := w.step(); err != nil {
					errs[i] = err
					stop.Store(true)
					return
				}
			}
		}()
	}
	wg.Wait()

	total := 0
	for _, w := range workers {
		total += w.ops
	}
	framework.Logf("randomized S3 workload: %d operations over %d workers", total, len(workers))
	for i, err := range errs {
		if err != nil {
			ginkgo.Fail(fmt.Sprintf("worker %d: %v\n\nreplay with S3_E2E_SEED=%d\n\nlast operations:\n%s",
				i, err, cfg.seed, log.tail()))
		}
	}

	ginkgo.By("every worker deletes its buckets and accounts")
	for _, w := range workers {
		framework.ExpectNoError(w.cleanup())
	}
}

// step draws and performs one operation, records it, and returns the first
// disagreement with the model.
func (w *s3Worker) step() error {
	type op struct {
		name   string
		weight int
		run    func() (string, error)
	}
	ops := []op{
		{"CreateBucket", 4, w.createBucket},
		{"DeleteBucket", 1, w.deleteBucket},
		{"PutObject", 25, w.putObject},
		{"MultipartUpload", 3, w.putMultipart},
		{"GetObject", 20, w.getObject},
		{"GetObjectRange", 6, w.getRange},
		{"HeadObject", 6, w.headObject},
		{"GetMissingObject", 3, w.getMissing},
		{"ListObjects", 8, w.listObjects},
		{"CopyObject", 5, w.copyObject},
		{"DeleteObject", 6, w.deleteObject},
		{"DeleteObjects", 2, w.deleteObjects},
		{"ListBuckets", 3, w.listBuckets},
		{"OutsiderProbe", 4, w.outsiderProbe},
		{"ForeignProbe", 2, w.foreignProbe},
	}
	total := 0
	for _, o := range ops {
		total += o.weight
	}
	pick := w.rng.IntN(total)
	chosen := ops[0]
	for _, o := range ops {
		if pick < o.weight {
			chosen = o
			break
		}
		pick -= o.weight
	}

	started := time.Now()
	detail, err := chosen.run()
	w.ops++
	result := detail
	if err != nil {
		result = fmt.Sprintf("%s: FAILED: %v", detail, err)
	}
	w.log.add(opRecord{at: started, worker: w.id, op: chosen.name, result: result})
	if err != nil {
		return fmt.Errorf("%s %s: %w", chosen.name, detail, err)
	}
	return nil
}

// bucket picks one of the worker's buckets, creating the first when it has
// none.
func (w *s3Worker) bucket() (string, error) {
	if len(w.buckets) == 0 {
		if _, err := w.createBucket(); err != nil {
			return "", err
		}
	}
	names := make([]string, 0, len(w.buckets))
	for name := range w.buckets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names[w.rng.IntN(len(names))], nil
}

// existingKey picks a bucket with at least one object and one of its keys.
func (w *s3Worker) existingKey() (string, string, bool) {
	var candidates []string
	for name, objects := range w.buckets {
		if len(objects) > 0 {
			candidates = append(candidates, name)
		}
	}
	if len(candidates) == 0 {
		return "", "", false
	}
	sort.Strings(candidates)
	bucket := candidates[w.rng.IntN(len(candidates))]
	keys := sortedKeys(w.buckets[bucket])
	return bucket, keys[w.rng.IntN(len(keys))], true
}

func sortedKeys(objects map[string][]byte) []string {
	keys := make([]string, 0, len(objects))
	for k := range objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// newKey draws a key of up to three directory levels. Directory names start
// with d and object names with f, so no key is ever both a file and a
// directory on the gateway's POSIX backend. Some names carry characters that
// need URL encoding.
func (w *s3Worker) newKey() string {
	segment := func(lead string) string {
		alphabet := "abcdefghijklmnopqrstuvwxyz0123456789-_."
		n := 1 + w.rng.IntN(12)
		var b strings.Builder
		b.WriteString(lead)
		for range n {
			b.WriteByte(alphabet[w.rng.IntN(len(alphabet))])
		}
		switch w.rng.IntN(10) {
		case 0:
			b.WriteString(" +x")
		case 1:
			b.WriteString("-ü")
		}
		return b.String()
	}
	var parts []string
	for range w.rng.IntN(4) {
		parts = append(parts, segment("d"))
	}
	return strings.Join(append(parts, segment("f")), "/")
}

// newSize draws an object size: mostly small, sometimes zero, rarely several
// MiB.
func (w *s3Worker) newSize() int {
	switch r := w.rng.Float64(); {
	case r < 0.05:
		return 0
	case r < 0.5:
		return w.rng.IntN(4 << 10)
	case r < 0.85:
		return w.rng.IntN(256 << 10)
	case r < 0.97:
		return w.rng.IntN(2 << 20)
	default:
		return 2<<20 + w.rng.IntN(4<<20)
	}
}

func (w *s3Worker) newData(n int) []byte {
	data := make([]byte, n)
	for i := 0; i < n; i += 8 {
		v := w.rng.Uint64()
		for j := 0; j < 8 && i+j < n; j++ {
			data[i+j] = byte(v >> (8 * j))
		}
	}
	return data
}

func (w *s3Worker) createBucket() (string, error) {
	if len(w.buckets) >= maxBucketsPerWorker {
		return w.listBuckets()
	}
	name := fmt.Sprintf("%s-w%d-b%d", w.env.prefix, w.id, w.created)
	w.created++
	if err := createBucket(w.asOwner, name); err != nil {
		return name, err
	}
	w.buckets[name] = map[string][]byte{}
	w.foreign.Store(name, w.id)
	return name, expectBucketReady(w.env, name)
}

func (w *s3Worker) deleteBucket() (string, error) {
	if len(w.buckets) == 0 {
		return w.createBucket()
	}
	bucket, err := w.bucket()
	if err != nil {
		return "", err
	}
	objects := w.buckets[bucket]
	if len(objects) > 0 {
		if err := expectS3Error(deleteBucket(w.asOwner, bucket), "BucketNotEmpty"); err != nil {
			return bucket, err
		}
		if err := deleteObjects(w.asOwner, bucket, sortedKeys(objects)); err != nil {
			return bucket, err
		}
	}
	if err := deleteBucket(w.asOwner, bucket); err != nil {
		return bucket, err
	}
	delete(w.buckets, bucket)
	w.foreign.Delete(bucket)
	return bucket, waitForBucketGone(w.env, bucket)
}

func (w *s3Worker) putObject() (string, error) {
	bucket, err := w.bucket()
	if err != nil {
		return "", err
	}
	objects := w.buckets[bucket]
	key := w.newKey()
	// Overwrite an existing key a third of the time, and always when the
	// bucket is full.
	if len(objects) > 0 && (len(objects) >= maxObjectsPerBucket || w.rng.IntN(3) == 0) {
		keys := sortedKeys(objects)
		key = keys[w.rng.IntN(len(keys))]
	}
	data := w.newData(w.newSize())
	detail := fmt.Sprintf("%s/%s (%d bytes)", bucket, key, len(data))
	if err := putObject(w.asOwner, bucket, key, data); err != nil {
		return detail, err
	}
	objects[key] = data
	return detail, nil
}

func (w *s3Worker) putMultipart() (string, error) {
	bucket, err := w.bucket()
	if err != nil {
		return "", err
	}
	if len(w.buckets[bucket]) >= maxObjectsPerBucket {
		return w.deleteObject()
	}
	key := w.newKey()
	data := w.newData(s3PartSize + 1 + w.rng.IntN(2*s3PartSize))
	detail := fmt.Sprintf("%s/%s (%d bytes)", bucket, key, len(data))
	if err := putMultipart(w.asOwner, bucket, key, data); err != nil {
		return detail, err
	}
	w.buckets[bucket][key] = data
	return detail, nil
}

func (w *s3Worker) getObject() (string, error) {
	bucket, key, ok := w.existingKey()
	if !ok {
		return w.putObject()
	}
	return bucket + "/" + key, expectObject(w.asOwner, bucket, key, w.buckets[bucket][key])
}

func (w *s3Worker) getRange() (string, error) {
	bucket, key, ok := w.existingKey()
	if !ok || len(w.buckets[bucket][key]) == 0 {
		return w.putObject()
	}
	want := w.buckets[bucket][key]
	first := w.rng.IntN(len(want))
	last := first + w.rng.IntN(len(want)-first)
	detail := fmt.Sprintf("%s/%s bytes=%d-%d", bucket, key, first, last)
	got, err := getObject(w.asOwner, bucket, key, aws.String(fmt.Sprintf("bytes=%d-%d", first, last)))
	if err != nil {
		return detail, err
	}
	return detail, compareBytes(detail, got, want[first:last+1])
}

func (w *s3Worker) headObject() (string, error) {
	bucket, key, ok := w.existingKey()
	if !ok {
		return w.putObject()
	}
	return bucket + "/" + key, expectObjectSize(w.asOwner, bucket, key, int64(len(w.buckets[bucket][key])))
}

func (w *s3Worker) getMissing() (string, error) {
	bucket, err := w.bucket()
	if err != nil {
		return "", err
	}
	key := w.newKey() + "-missing"
	return bucket + "/" + key, expectS3Error(getObjectErr(w.asOwner, bucket, key), "NoSuchKey")
}

func (w *s3Worker) listObjects() (string, error) {
	bucket, err := w.bucket()
	if err != nil {
		return "", err
	}
	objects := w.buckets[bucket]
	// List the whole bucket or under the directory part of one of its keys.
	prefix := ""
	if len(objects) > 0 && w.rng.IntN(2) == 0 {
		keys := sortedKeys(objects)
		key := keys[w.rng.IntN(len(keys))]
		if i := strings.LastIndex(key, "/"); i >= 0 {
			prefix = key[:i+1]
		}
	}
	detail := fmt.Sprintf("%s prefix %q", bucket, prefix)
	got, err := listKeys(w.asOwner, bucket, prefix)
	if err != nil {
		return detail, err
	}
	var want []string
	for _, key := range sortedKeys(objects) {
		if strings.HasPrefix(key, prefix) {
			want = append(want, key)
		}
	}
	sort.Strings(got)
	if !slices.Equal(got, want) {
		return detail, fmt.Errorf("listed %d keys %v, the model holds %d %v", len(got), got, len(want), want)
	}
	return detail, nil
}

func (w *s3Worker) copyObject() (string, error) {
	srcBucket, srcKey, ok := w.existingKey()
	if !ok {
		return w.putObject()
	}
	dstBucket, err := w.bucket()
	if err != nil {
		return "", err
	}
	if len(w.buckets[dstBucket]) >= maxObjectsPerBucket {
		return w.deleteObject()
	}
	dstKey := w.newKey()
	detail := fmt.Sprintf("%s/%s to %s/%s", srcBucket, srcKey, dstBucket, dstKey)
	if err := copyObject(w.asOwner, srcBucket, srcKey, dstBucket, dstKey); err != nil {
		return detail, err
	}
	w.buckets[dstBucket][dstKey] = w.buckets[srcBucket][srcKey]
	return detail, expectObject(w.asOwner, dstBucket, dstKey, w.buckets[dstBucket][dstKey])
}

func (w *s3Worker) deleteObject() (string, error) {
	bucket, key, ok := w.existingKey()
	if !ok {
		return w.putObject()
	}
	detail := bucket + "/" + key
	if err := deleteObject(w.asOwner, bucket, key); err != nil {
		return detail, err
	}
	delete(w.buckets[bucket], key)
	return detail, expectS3Error(getObjectErr(w.asOwner, bucket, key), "NoSuchKey")
}

func (w *s3Worker) deleteObjects() (string, error) {
	bucket, _, ok := w.existingKey()
	if !ok {
		return w.putObject()
	}
	keys := sortedKeys(w.buckets[bucket])
	w.rng.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	keys = keys[:1+w.rng.IntN(len(keys))]
	detail := fmt.Sprintf("%s (%d keys)", bucket, len(keys))
	if err := deleteObjects(w.asOwner, bucket, keys); err != nil {
		return detail, err
	}
	for _, key := range keys {
		delete(w.buckets[bucket], key)
	}
	return detail, nil
}

// listBuckets checks that the owner lists exactly its own buckets among the
// run's, and that each of them is a Ready S3Bucket.
func (w *s3Worker) listBuckets() (string, error) {
	got, err := listBucketNames(w.asOwner, w.env.prefix)
	if err != nil {
		return "", err
	}
	want := make([]string, 0, len(w.buckets))
	for name := range w.buckets {
		want = append(want, name)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		return "", fmt.Errorf("the owner lists %v, the model holds %v", got, want)
	}
	for _, name := range want {
		if err := expectBucketReady(w.env, name); err != nil {
			return name, err
		}
	}
	return fmt.Sprintf("%d buckets", len(want)), nil
}

// outsiderProbe checks that the user-role account can neither create a
// bucket nor read, list, or write the owner's.
func (w *s3Worker) outsiderProbe() (string, error) {
	bucket, err := w.bucket()
	if err != nil {
		return "", err
	}
	if err := expectS3Error(createBucket(w.asOut, fmt.Sprintf("%s-w%d-denied", w.env.prefix, w.id)),
		"AccessDenied"); err != nil {
		return "CreateBucket", err
	}
	_, err = listKeys(w.asOut, bucket, "")
	if err := expectS3Error(err, "AccessDenied"); err != nil {
		return bucket + " ListObjects", err
	}
	if err := expectS3Error(putObject(w.asOut, bucket, "f-outsider", []byte("x")), "AccessDenied"); err != nil {
		return bucket + " PutObject", err
	}
	if bucket, key, ok := w.existingKey(); ok {
		if err := expectS3Error(getObjectErr(w.asOut, bucket, key), "AccessDenied"); err != nil {
			return bucket + "/" + key + " GetObject", err
		}
	}
	return bucket, nil
}

// foreignProbe checks that the owner cannot list another worker's bucket.
// That bucket may be deleted concurrently, so NoSuchBucket is accepted too.
func (w *s3Worker) foreignProbe() (string, error) {
	var others []string
	w.foreign.Range(func(name, id any) bool {
		if id.(int) != w.id {
			others = append(others, name.(string))
		}
		return true
	})
	if len(others) == 0 {
		return w.listBuckets()
	}
	sort.Strings(others)
	bucket := others[w.rng.IntN(len(others))]
	_, err := listKeys(w.asOwner, bucket, "")
	if code := s3ErrorCode(err); code == "AccessDenied" || code == "NoSuchBucket" {
		return bucket, nil
	}
	return bucket, fmt.Errorf("listing another owner's bucket answered %v", err)
}

func (w *s3Worker) cleanup() error {
	for _, bucket := range sortedKeysOfBuckets(w.buckets) {
		if err := emptyAndDeleteBucket(w.asOwner, bucket); err != nil {
			return fmt.Errorf("worker %d: %w", w.id, err)
		}
		if err := waitForBucketGone(w.env, bucket); err != nil {
			return err
		}
	}
	for _, account := range []s3Account{w.owner, w.outside} {
		if err := w.env.gw.deleteUser(w.env.root, account.access); err != nil {
			return err
		}
		delete(w.env.users, account.access)
	}
	return nil
}

func sortedKeysOfBuckets(buckets map[string]map[string][]byte) []string {
	names := make([]string, 0, len(buckets))
	for name := range buckets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}
