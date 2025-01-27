package reporting

import (
	"compress/flate"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/alexmullins/zip"
	"github.com/pkg/errors"
	"www.velocidex.com/golang/velociraptor/accessors"
	"www.velocidex.com/golang/velociraptor/actions"
	actions_proto "www.velocidex.com/golang/velociraptor/actions/proto"
	config_proto "www.velocidex.com/golang/velociraptor/config/proto"
	"www.velocidex.com/golang/velociraptor/file_store/csv"
	"www.velocidex.com/golang/velociraptor/json"
	"www.velocidex.com/golang/velociraptor/logging"
	"www.velocidex.com/golang/velociraptor/paths"
	"www.velocidex.com/golang/velociraptor/uploads"
	"www.velocidex.com/golang/velociraptor/utils"
	vql_subsystem "www.velocidex.com/golang/velociraptor/vql"
	"www.velocidex.com/golang/vfilter"

	concurrent_zip "github.com/Velocidex/zip"
)

type MemberWriter struct {
	io.WriteCloser
	writer_wg *sync.WaitGroup
}

// Keep track of all members that are closed to allow the zip to be
// written properly.
func (self *MemberWriter) Close() error {
	err := self.WriteCloser.Close()
	self.writer_wg.Done()
	return err
}

type Container struct {
	config_obj *config_proto.Config

	// The underlying file writer
	fd      io.WriteCloser
	writer  *utils.TeeWriter
	sha_sum hash.Hash

	level int

	// We write data to this zip file using the concurrent zip
	// implementation.
	zip *concurrent_zip.Writer

	// If a password is set, we create a new zip file here, and a
	// member within it then redirect the zip above to write on
	// it.
	delegate_zip *zip.Writer
	delegate_fd  io.Writer

	// manage orderly shutdown of the container.
	mu sync.Mutex

	// Keep track of all writers so we can safely close the container.
	writer_wg sync.WaitGroup
	closed    bool
}

func (self *Container) Create(name string, mtime time.Time) (io.WriteCloser, error) {
	self.writer_wg.Add(1)
	header := &concurrent_zip.FileHeader{
		Name:     name,
		Method:   concurrent_zip.Deflate,
		Modified: mtime,
	}

	if self.level == 0 {
		header.Method = concurrent_zip.Store
	}

	writer, err := self.zip.CreateHeader(header)
	if err != nil {
		return nil, err
	}

	return &MemberWriter{
		WriteCloser: writer,
		writer_wg:   &self.writer_wg,
	}, nil
}

func (self *Container) StoreArtifact(
	config_obj *config_proto.Config,
	ctx context.Context,
	scope vfilter.Scope,
	query *actions_proto.VQLRequest,
	format string) (err error) {

	query_log := actions.QueryLog.AddQuery(query.VQL)
	defer query_log.Close()

	vql, err := vfilter.Parse(query.VQL)
	if err != nil {
		return err
	}

	artifact_name := query.Name

	// Dont store un-named queries but run them anyway.
	if artifact_name == "" {
		for range vql.Eval(ctx, scope) {
		}
		return nil
	}

	// The name to use in the zip file to store results from this artifact
	path_manager := paths.NewContainerPathManager(artifact_name)
	fd, err := self.Create(path_manager.Path(), time.Time{})
	if err != nil {
		return err
	}

	// Preserve the error for our caller.
	defer func() {
		err_ := fd.Close()
		if err == nil {
			err = err_
		}
	}()

	// Optionally include CSV in the output
	var csv_writer *csv.CSVWriter
	if format == "csv" {
		csv_fd, err := self.Create(path_manager.CSVPath(), time.Time{})
		if err != nil {
			return err
		}

		csv_writer = csv.GetCSVAppender(config_obj,
			scope, csv_fd, true /* write_headers */)

		// Preserve the error for our caller.
		defer func() {
			csv_writer.Close()
			err_ := csv_fd.Close()
			if err == nil {
				err = err_
			}
		}()
	}

	// Store as line delimited JSON
	marshaler := vql_subsystem.MarshalJsonl(scope)
	for row := range vql.Eval(ctx, scope) {
		select {
		case <-ctx.Done():
			return

		default:
			// Re-serialize it as compact json.
			serialized, err := marshaler([]vfilter.Row{row})
			if err != nil {
				continue
			}

			_, err = fd.Write(serialized)
			if err != nil {
				return errors.WithStack(err)
			}

			if csv_writer != nil {
				csv_writer.Write(row)
			}
		}
	}

	return nil
}

func sanitize_upload_name(store_as_name string) string {
	components := []string{}
	// Normalize and clean up the path so the zip file is more
	// usable by fragile zip programs like Windows explorer.
	for _, component := range utils.SplitComponents(store_as_name) {
		if component == "." || component == ".." {
			continue
		}
		components = append(components, sanitize(component))
	}

	// Zip members must not have absolute paths.
	return path.Join(components...)
}

func sanitize(component string) string {
	component = strings.Replace(component, ":", "", -1)
	component = strings.Replace(component, "?", "", -1)
	return component
}

func (self *Container) Upload(
	ctx context.Context,
	scope vfilter.Scope,
	filename *accessors.OSPath,
	accessor string,
	store_as_name string,
	expected_size int64,
	mtime time.Time,
	atime time.Time,
	ctime time.Time,
	btime time.Time,
	reader io.Reader) (*uploads.UploadResponse, error) {

	if store_as_name == "" {
		store_as_name = accessors.MustNewGenericOSPath(accessor).Append(filename.Components...).String()
	}

	sanitized_name := sanitize_upload_name(store_as_name)

	scope.Log("Collecting file %s into %s (%v bytes)",
		filename.String(), store_as_name, expected_size)

	// Try to collect sparse files if possible
	result, err := self.maybeCollectSparseFile(
		ctx, scope, reader, store_as_name, sanitized_name, mtime)
	if err == nil {
		return result, nil
	}

	writer, err := self.Create(sanitized_name, mtime)
	if err != nil {
		return nil, err
	}
	defer writer.Close()

	sha_sum := sha256.New()
	md5_sum := md5.New()

	n, err := utils.Copy(ctx, utils.NewTee(writer, sha_sum, md5_sum), reader)
	if err != nil {
		return &uploads.UploadResponse{
			Error: err.Error(),
		}, err
	}

	return &uploads.UploadResponse{
		Path:   sanitized_name,
		Size:   uint64(n),
		Sha256: hex.EncodeToString(sha_sum.Sum(nil)),
		Md5:    hex.EncodeToString(md5_sum.Sum(nil)),
	}, nil
}

func (self *Container) maybeCollectSparseFile(
	ctx context.Context,
	scope vfilter.Scope,
	reader io.Reader, store_as_name, sanitized_name string, mtime time.Time) (
	*uploads.UploadResponse, error) {

	// Can the reader produce ranges?
	range_reader, ok := reader.(uploads.RangeReader)
	if !ok {
		return nil, errors.New("Not supported")
	}

	writer, err := self.Create(sanitized_name, mtime)
	if err != nil {
		return nil, err
	}
	defer writer.Close()

	sha_sum := sha256.New()
	md5_sum := md5.New()

	// The byte count we write to the output file.
	count := 0

	// An index array for sparse files.
	index := &actions_proto.Index{}
	is_sparse := false

	for _, rng := range range_reader.Ranges() {
		file_length := rng.Length
		if rng.IsSparse {
			file_length = 0
		}

		index.Ranges = append(index.Ranges,
			&actions_proto.Range{
				FileOffset:     int64(count),
				OriginalOffset: rng.Offset,
				FileLength:     file_length,
				Length:         rng.Length,
			})

		if rng.IsSparse {
			is_sparse = true
			continue
		}

		_, err = range_reader.Seek(rng.Offset, io.SeekStart)
		if err != nil {
			return &uploads.UploadResponse{
				Error: err.Error(),
			}, err
		}

		run_writer := utils.NewTee(writer, sha_sum, md5_sum)
		n, err := utils.CopyN(ctx, run_writer, range_reader, rng.Length)
		if err != nil {
			return &uploads.UploadResponse{
				Error: err.Error(),
			}, err
		}

		// We were unable to fully copy this run - this could indicate
		// an issue with decompression of the ntfs for
		// example. However we still need to maintain alignment here
		// so we pad with zeros.
		if int64(n) < rng.Length {
			scope.Log("Unable to fully copy range %v in %v - padding %v bytes",
				rng, store_as_name, rng.Length-int64(n))
			_, _ = utils.CopyN(
				ctx, run_writer, utils.ZeroReader{}, rng.Length-int64(n))
		}

		count += n
	}

	// If there were any sparse runs, create an index.
	if is_sparse {
		writer, err := self.Create(sanitized_name+".idx", time.Time{})
		if err != nil {
			return nil, err
		}
		defer writer.Close()

		serialized, err := json.Marshal(index)
		if err != nil {
			return &uploads.UploadResponse{
				Error: err.Error(),
			}, err
		}

		_, err = writer.Write(serialized)
		if err != nil {
			return &uploads.UploadResponse{
				Error: err.Error(),
			}, err
		}
	}

	return &uploads.UploadResponse{
		Path:   sanitized_name,
		Size:   uint64(count),
		Sha256: hex.EncodeToString(sha_sum.Sum(nil)),
		Md5:    hex.EncodeToString(md5_sum.Sum(nil)),
	}, nil
}

func (self *Container) IsClosed() bool {
	self.mu.Lock()
	defer self.mu.Unlock()

	return self.closed
}

// Close the underlying container zip (and write central
// directories). It is ok to call this multiple times.
func (self *Container) Close() error {
	self.mu.Lock()
	defer self.mu.Unlock()

	if self.closed {
		return nil
	}
	self.closed = true

	// Wait for all outstanding writers to finish before we close the
	// zip file.
	self.writer_wg.Wait()

	self.zip.Close()

	if self.delegate_zip != nil {
		self.delegate_zip.Close()
	}

	// Only report the hash if we actually wrote something (few bytes
	// are always written for the zip header).
	if self.writer.Count() > 50 {
		logger := logging.GetLogger(self.config_obj, &logging.GUIComponent)
		logger.Info("Container hash %v", hex.EncodeToString(self.sha_sum.Sum(nil)))
	}
	return self.fd.Close()
}

func NewContainer(
	config_obj *config_proto.Config,
	path string, password string, level int64) (*Container, error) {
	fd, err := os.OpenFile(
		path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return nil, err
	}

	if level < 0 || level > 9 {
		level = 5
	}

	sha_sum := sha256.New()

	result := &Container{
		config_obj: config_obj,
		fd:         fd,
		sha_sum:    sha_sum,
		writer:     utils.NewTee(fd, sha_sum),
		level:      int(level),
	}

	// We need to build a protected container.
	if password != "" {
		result.delegate_zip = zip.NewWriter(result.writer)

		// We are writing a zip file into here - no need to
		// compress.
		fh := &zip.FileHeader{
			Name:   "data.zip",
			Method: zip.Store,
		}
		fh.SetPassword(password)
		result.delegate_fd, err = result.delegate_zip.CreateHeader(fh)
		if err != nil {
			return nil, err
		}

		result.zip = concurrent_zip.NewWriter(result.delegate_fd)
	} else {
		result.zip = concurrent_zip.NewWriter(result.writer)
		result.zip.RegisterCompressor(
			zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
				return flate.NewWriter(out, int(level))
			})
	}

	return result, nil
}

// Turns os.Stdout into into file_store.WriteSeekCloser
type StdoutWrapper struct {
	io.Writer
}

func (self *StdoutWrapper) Seek(offset int64, whence int) (int64, error) {
	return 0, nil
}

func (self *StdoutWrapper) Close() error {
	return nil
}

func (self *StdoutWrapper) Truncate(offset int64) error {
	return nil
}
