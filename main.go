// Command ts-trail uploads Tailscale SSH session recordings to an S3 bucket.
// It deletes uploaded files.
package main

import (
	"archive/zip"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"golang.org/x/sync/errgroup"
)

func main() {
	log.SetFlags(0)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	args := runArgs{}
	flag.StringVar(&args.bucket, "bucket", args.bucket, "s3 bucket `name`")
	flag.StringVar(&args.prefix, "prefix", args.prefix, "s3 key prefix; defaults to hostname if empty")
	flag.BoolVar(&args.install, "install", args.install, "install systemd service")
	flag.Parse()
	if err := run(ctx, args); err != nil {
		log.Fatal(err)
	}
}

type runArgs struct {
	bucket  string
	prefix  string
	install bool
}

func run(ctx context.Context, args runArgs) error {
	if args.bucket == "" {
		return errors.New("bucket name must be set")
	}
	if os.Getuid() != 0 {
		return errors.New("this program requires root privileges")
	}
	cfg, err := loadDefaultConfig(ctx)
	if err != nil {
		return err
	}
	s3client := s3.NewFromConfig(cfg)
	if args.install {
		return installService(args, s3client)
	}
	cmd := exec.Command("journalctl", "-u", "tailscaled", "-a", "-f", "-o", "json")
	if b, _ := exec.Command("journalctl", "-h").CombinedOutput(); bytes.Contains(b, []byte("--output-fields")) {
		cmd.Args = append(cmd.Args, "--output-fields=MESSAGE,_PID,_HOSTNAME")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer cmd.Wait()
	defer cmd.Process.Signal(os.Interrupt)

	screenRecordings := make(chan *recordingTrackRecord, 1)
	group, ctx := errgroup.WithContext(ctx)
	group.Go(func() error { return uploadRecordings(ctx, s3client, args.bucket, screenRecordings) })
	group.Go(func() error {
		defer close(screenRecordings)
		prevLines := &recBuffer{}
		bld := new(strings.Builder)
		dec := json.NewDecoder(stdout)
		for {
			var rec logRecord
			if err := dec.Decode(&rec); err != nil {
				if err == io.EOF {
					return cmd.Wait()
				}
				return err
			}
			if s := lineSessionID(rec.Msg); s == "" {
				continue
			} else {
				rec.sid = s
			}
			if !strings.Contains(rec.Msg, recordingPrefix) {
				prevLines.add(rec)
				continue
			}
			bld.Reset()
			fmt.Fprintf(bld, "session %s at %s\n", rec.sid, rec.Hostname)
			var text string // message text without prefix
			for _, r := range append(prevLines.sidRecords(rec.sid), rec) {
				var ok bool
				_, text, ok = strings.Cut(r.Msg, "): ")
				if !ok {
					continue
				}
				bld.WriteString(text)
				bld.WriteByte('\n')
			}
			if !strings.HasPrefix(text, recordingPrefix) || !strings.HasSuffix(text, ".cast") {
				continue
			}
			fileName := filepath.Clean(strings.TrimPrefix(text, recordingPrefix))
			if !filepath.IsAbs(fileName) {
				continue
			}
			prefix := rec.Hostname
			if args.prefix != "" {
				prefix = args.prefix
			}
			select {
			case <-ctx.Done():
				// not exiting here to let decoder drain stdout
			case screenRecordings <- &recordingTrackRecord{
				pid:         rec.Pid,
				sid:         rec.sid,
				fileName:    fileName,
				prefix:      prefix,
				description: bld.String(),
			}:
			}
		}
	})
	return group.Wait()
}

func uploadRecordings(ctx context.Context, s3client *s3.Client, bucket string, screenRecordings <-chan *recordingTrackRecord) error {
	tf := new(trackedFiles)
	group, ctx := errgroup.WithContext(ctx)
	ctx, cancel := context.WithCancel(ctx)
	group.Go(func() error { tf.loop(ctx, s3client, bucket); return nil })
	group.Go(func() error {
		defer cancel()
		for rec := range screenRecordings {
			if rec.pid <= 0 {
				log.Printf("non-positive pid %d for file %q", rec.pid, rec.fileName)
				continue
			}
			if err := tf.track(rec); err != nil {
				log.Printf("failed to start tracking of screen recording file %q (pid %d): %v", rec.fileName, rec.pid, err)
			} else {
				log.Printf("started tracking screen recording file %q (pid %d)", rec.fileName, rec.pid)
			}
		}
		return nil
	})
	return group.Wait()
}

type trackedFiles struct {
	mu sync.Mutex
	m  map[string]*recordingTrackRecord
}

func (tf *trackedFiles) track(rec *recordingTrackRecord) error {
	dir := fmt.Sprintf("/proc/%d/fd", rec.pid)
	links, err := func() ([]string, error) {
		f, err := os.Open(dir)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return f.Readdirnames(0)
	}()
	if err != nil {
		return err
	}
	for _, link := range links {
		name, err := os.Readlink(filepath.Join(dir, link))
		if errors.Is(err, fs.ErrNotExist) { // scanning /proc is inherently racy
			continue
		}
		if name != rec.fileName {
			continue
		}
		fd, err := strconv.Atoi(link)
		if err != nil {
			return fmt.Errorf("parsing fd number from %q", name)
		}
		rec.fd = fd
		break
	}

	if rec.fd == 0 {
		// in case we lost the race with tailscaled closing the file,
		// remap fd number to make it explicitly fail the stillHeld check
		rec.fd = -1
	}

	if ok, err := isPidTailscaled(rec.pid); err != nil || !ok {
		// maybe tailscaled has restarted, so instead of skipping this file,
		// mark it for uploading right away by remapping pid to make it
		// explicitly fail the stillHeld check
		rec.pid = -1
	}
	f, err := os.Open(rec.fileName)
	if err != nil {
		return err
	}
	rec.file = f

	tf.mu.Lock()
	defer tf.mu.Unlock()
	if tf.m == nil {
		tf.m = make(map[string]*recordingTrackRecord)
	}
	tf.m[rec.fileName] = rec
	return nil
}

func (tf *trackedFiles) loop(ctx context.Context, s3client *s3.Client, bucket string) {
	tracked := func() []*recordingTrackRecord {
		tf.mu.Lock()
		defer tf.mu.Unlock()
		if len(tf.m) == 0 {
			return nil
		}
		out := make([]*recordingTrackRecord, len(tf.m))
		var i int
		for k := range tf.m {
			out[i] = tf.m[k]
			i++
		}
		return out
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
		for _, rec := range tracked() {
			if stillHeld(rec) {
				continue
			}
			log.Printf("file %q is not held by the tailscaled process anymore, uploading it as %q", rec.fileName, rec.s3key())
			if err := upload(ctx, s3client, bucket, rec); err != nil {
				log.Printf("file %q upload: %v", rec.fileName, err)
				continue
			} else {
				_ = os.Remove(rec.fileName)
			}
			tf.forget(rec.fileName)
		}
	}
}

func (tf *trackedFiles) forget(name string) {
	tf.mu.Lock()
	defer tf.mu.Unlock()
	rec, ok := tf.m[name]
	delete(tf.m, name)
	if !ok {
		return
	}
	_ = rec.file.Close()
}

func upload(ctx context.Context, s3client *s3.Client, bucket string, rec *recordingTrackRecord) error {
	if _, err := rec.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	tf, err := os.CreateTemp("", "screencast-*.zip")
	if err != nil {
		return err
	}
	defer tf.Close()
	defer os.Remove(tf.Name())
	now := time.Now()
	zwr := zip.NewWriter(tf)
	w, err := zwr.CreateHeader(&zip.FileHeader{
		Name:     filepath.Base(rec.fileName),
		Method:   zip.Deflate,
		Modified: now,
	})
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, rec.file); err != nil {
		return err
	}
	w, err = zwr.CreateHeader(&zip.FileHeader{
		Name:     "metadata.txt",
		Method:   zip.Deflate,
		Modified: now,
	})
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, rec.description); err != nil {
		return err
	}
	if err := zwr.Close(); err != nil {
		return err
	}
	if _, err := tf.Seek(0, io.SeekStart); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	key := rec.s3key()
	_, err = s3client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &bucket,
		Key:    &key,
		ACL:    types.ObjectCannedACLPrivate,
		Body:   tf,
	})
	return err
}

func stillHeld(rec *recordingTrackRecord) bool {
	if rec.pid == -1 || rec.fd == -1 {
		return false
	}
	link, _ := os.Readlink(fmt.Sprintf("/proc/%d/fd/%d", rec.pid, rec.fd))
	return link == rec.fileName
}

func isPidTailscaled(pid int) (bool, error) {
	name, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return false, err
	}
	return filepath.Base(name) == "tailscaled", nil
}

type recordingTrackRecord struct {
	file        *os.File
	pid         int // tailscaled pid
	fd          int // which file descriptor tailscaled has assigned to this file
	sid         string
	prefix      string
	fileName    string
	description string
}

func (r *recordingTrackRecord) s3key() string { return path.Join(r.prefix, r.sid+".zip") }

type logRecord struct {
	Pid      int    `json:"_PID,string"`
	Msg      string `json:"MESSAGE"`
	Hostname string `json:"_HOSTNAME"`
	sid      string // session id extracted from Msg
}

type recBuffer struct {
	a [10]logRecord
	i int
}

func (r *recBuffer) add(rec logRecord) {
	r.a[r.i] = rec
	r.i = (r.i + 1) % len(r.a)
}

func (r *recBuffer) sidRecords(sid string) []logRecord {
	if sid == "" {
		return nil
	}
	var out []logRecord
	// both loops' bodies must be the same
	for i := r.i; i < len(r.a); i++ {
		if r.a[i].Pid == 0 || r.a[i].sid != sid {
			continue
		}
		out = append(out, r.a[i])
		r.a[i] = logRecord{}
	}
	for i := 0; i < r.i; i++ {
		if r.a[i].Pid == 0 || r.a[i].sid != sid {
			continue
		}
		out = append(out, r.a[i])
		r.a[i] = logRecord{}
	}
	return out
}

func lineSessionID(line string) string {
	if !strings.HasPrefix(line, sessionLinePrefix) {
		return ""
	}
	i := strings.IndexByte(line, ')')
	if i == -1 {
		return ""
	}
	return line[len(sessionLinePrefix):i]
}

const sessionLinePrefix = "ssh-session("
const recordingPrefix = "starting asciinema recording to "

func loadDefaultConfig(ctx context.Context) (aws.Config, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return config.LoadDefaultConfig(ctx)
}

func installService(args runArgs, s3client *s3.Client) error {
	region := os.Getenv("AWS_REGION")
	if region == "" {
		return errors.New("please run with AWS_REGION environment configured" +
			"\nhint: AWS_REGION=... sudo --preserve-env=AWS_REGION " + strings.Join(os.Args, " "))
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if self == "" {
		return errors.New("cannot resolve absolute path of ifself")
	}
	err = func() error {
		key := path.Join(args.prefix, "ts-trail-install-upload-test")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := s3client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: &args.bucket,
			Key:    &key,
			ACL:    types.ObjectCannedACLPrivate,
			Body:   strings.NewReader("upload test, safe to delete\n"),
		})
		return err
	}()
	if err != nil {
		return fmt.Errorf("s3 bucket %q (prefix %q) access test failed: %w", args.bucket, args.prefix, err)
	}
	tpl := template.Must(template.New("").Parse(serviceTemplate)).Option("missingkey=error")
	const canonicalProgramPath = "/usr/local/bin/ts-trail"
	if self != canonicalProgramPath {
		cmd := exec.Command("install", "-v", self, canonicalProgramPath)
		cmd.Stderr = os.Stderr
		cmd.Stdout = os.Stdout
		log.Println(cmd)
		if err := cmd.Run(); err != nil {
			return err
		}
	}
	buf := new(bytes.Buffer)
	if err := tpl.Execute(buf, struct{ Binary, Bucket, Prefix, Region string }{
		Binary: canonicalProgramPath,
		Region: region,
		Bucket: args.bucket,
		Prefix: args.prefix}); err != nil {
		return fmt.Errorf("rendering service template: %w", err)
	}
	const unitPath = "/etc/systemd/system/ts-trail.service"
	if err := os.WriteFile(unitPath, buf.Bytes(), 0660); err != nil {
		return fmt.Errorf("writing %q: %w", unitPath, err)
	}
	log.Printf("wrote %q", unitPath)
	cmd := exec.Command("systemctl", "enable", "--now", unitPath)
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	log.Println(cmd)
	return cmd.Run()
}

//go:embed service.template
var serviceTemplate string
