/*
 * Copyright (c) 2019-2021. Abstrium SAS <team (at) pydio.com>
 * This file is part of Pydio Cells.
 *
 * Pydio Cells is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * Pydio Cells is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with Pydio Cells.  If not, see <http://www.gnu.org/licenses/>.
 *
 * The latest code can be found at <https://pydio.com>.
 */

package bleve

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/xid"

	bleve "github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/index/scorch"
	"github.com/blevesearch/bleve/v2/index/upsidedown/store/boltdb"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/search/query"
	"github.com/pydio/cells/v4/common/dao"
	"github.com/pydio/cells/v4/common/utils/configx"
	"github.com/pydio/cells/v4/common/utils/uuid"
)

const (
	BufferedChanSize = 10000
	MinRotationSize  = 68 * 1024
)

var (
	UnitTestEnv = false
)

type IndexDAO interface {
	DAO
	dao.IndexDAO
}

// Indexer is the syslog specific implementation of the Log server
type Indexer struct {
	DAO
	searchIndex bleve.IndexAlias
	indexes     []bleve.Index
	cursor      int
	indexPath   string

	opened      bool
	inserts     chan interface{}
	deletes     chan interface{}
	forceFlush  chan bool
	insertsDone chan bool
	crtBatch    *bleve.Batch
	flushLock   *sync.Mutex

	codec          dao.IndexCodex
	serviceConfigs configx.Values
}

// NewIndexer creates and configures a default Bleve instance to store technical logs
// Setting rotationSize to -1 fully disables rotation
func NewIndexer(rd dao.DAO) (dao.IndexDAO, error) {

	d := rd.(DAO)
	conf := d.BleveConfig()
	if conf.RotationSize > -1 && conf.RotationSize < MinRotationSize {
		return nil, fmt.Errorf("use a rotation size bigger than %d", MinRotationSize)
	}
	server := &Indexer{
		DAO: d,
	}
	return server, nil
}

func (s *Indexer) Init(cfg configx.Values) error {
	if er := s.DAO.Init(cfg); er != nil {
		return er
	}
	s.serviceConfigs = cfg
	return s.Open(s.BleveConfig().BlevePath)
}

// Open lists all existing indexes and creates a writeable index on the active one
// and a composed index for searching. It calls watchInserts() to start watching for
// new logs
func (s *Indexer) Open(indexPath string) error {

	s.indexPath = indexPath
	mappingName := s.BleveConfig().MappingName

	s.searchIndex = bleve.NewIndexAlias()
	s.indexes = []bleve.Index{}
	s.flushLock = &sync.Mutex{}
	s.forceFlush = make(chan bool, 1)
	if UnitTestEnv {
		s.inserts = make(chan interface{})
		s.deletes = make(chan interface{})
	} else {
		s.inserts = make(chan interface{}, BufferedChanSize)
		s.deletes = make(chan interface{}, BufferedChanSize)
	}

	existing := s.listIndexes(true)
	if len(existing) == 0 {
		index, err := s.openOneIndex(s.indexPath, mappingName)
		if err != nil {
			return err
		}
		s.searchIndex.Add(index)
		s.indexes = append(s.indexes, index)
		s.cursor = 0
	} else {
		for _, iName := range existing {
			iPath := filepath.Join(filepath.Dir(s.indexPath), iName)
			if index, err := s.openOneIndex(iPath, mappingName); err == nil {
				s.indexes = append(s.indexes, index)
			} else {
				fmt.Println("[pydio.grpc.log] Cannot open bleve index", iPath, err)
			}
		}
		s.searchIndex.Add(s.indexes...)
		s.cursor = len(s.indexes) - 1
	}
	s.insertsDone = make(chan bool)
	s.opened = true

	if s.indexPath != "" && s.BleveConfig().RotationSize > -1 {
		s.rotateIfNeeded()
	}
	go s.watchInserts()
	return nil
}

func (s *Indexer) Close() error {
	if !s.opened {
		return nil
	}
	s.opened = false
	close(s.insertsDone)
	close(s.inserts)
	close(s.deletes)
	close(s.forceFlush)
	return nil
}

func (s *Indexer) InsertOne(ctx context.Context, data interface{}) error {

	if !s.opened {
		return nil
	}
	if UnitTestEnv { // blocking insert
		s.inserts <- data
	} else {
		select { // non-blocking insert
		case s.inserts <- data:
		default:
		}
	}
	return nil
}

func (s *Indexer) DeleteOne(ctx context.Context, data interface{}) error {

	if !s.opened {
		return nil
	}

	if UnitTestEnv { // blocking insert
		s.deletes <- data
	} else {
		select { // non-blocking insert
		case s.deletes <- data:
		default:
		}
	}
	return nil
}

func (s *Indexer) Flush() {

	if !s.opened {
		return
	}

	select { // non-blocking insert
	case s.forceFlush <- true:
	default:
	}
}

func (s *Indexer) DeleteMany(ctx context.Context, qu interface{}) (int32, error) {

	var q query.Query
	var str string
	var ok bool
	if str, ok = qu.(string); !ok {
		return 0, fmt.Errorf("DeleteMany expects a query string")
	} else if str == "" {
		return 0, fmt.Errorf("cannot pass an empty query for deletion")
	}
	q = bleve.NewQueryStringQuery(str)
	req := bleve.NewSearchRequest(q)
	req.Size = 1000
	var count int32

	idx := s.getWriteIndex()
	for {
		sr, err := idx.Search(req)
		if err != nil {
			fmt.Println(err)
			return 0, err
		}
		b := idx.NewBatch()
		for _, hit := range sr.Hits {
			b.Delete(hit.ID)
			count++
		}
		if err := idx.Batch(b); err != nil {
			return count, err
		}
		if sr.Total <= uint64(req.Size) {
			break
		}
	}

	return count, nil

}

func (s *Indexer) FindMany(ctx context.Context, query interface{}, offset, limit int32, customCodec dao.IndexCodex) (chan interface{}, error) {
	codec := s.codec
	if customCodec != nil {
		codec = customCodec
	}
	request, _, err := codec.BuildQuery(query, offset, limit)
	if err != nil {
		return nil, err
	}
	req, ok := request.(*bleve.SearchRequest)
	if !ok {
		return nil, fmt.Errorf("Unrecognized searchRequest type")
	}
	sr, er := s.searchIndex.SearchInContext(ctx, req)
	if er != nil {
		return nil, er
	}
	cRes := make(chan interface{})

	go func() {
		defer close(cRes)
		// Send hits
		for _, hit := range sr.Hits {
			if result, err := codec.Unmarshal(hit); err == nil {
				cRes <- result
			}
		}
		// Parse & send facets
		if fParser, ok := codec.(dao.FacetParser); ok {
			for _, facet := range sr.Facets {
				fParser.UnmarshalFacet(facet, cRes)
			}
		}
	}()
	return cRes, nil
}

func (s *Indexer) SetCodex(c dao.IndexCodex) {
	s.codec = c
}

func (s *Indexer) getWriteIndex() bleve.Index {
	return s.indexes[s.cursor]
}

func (s *Indexer) listIndexes(renameIfNeeded ...bool) (paths []string) {
	dirPath, base := filepath.Split(s.indexPath)
	dir, err := os.Open(dirPath)
	if err != nil {
		return
	}
	defer dir.Close()

	files, err := dir.Readdir(-1)
	if err != nil {
		return
	}

	for _, file := range files {
		if !file.IsDir() {
			continue
		}
		curBase := filepath.Base(file.Name())
		if curBase == base {
			paths = append(paths, curBase)
		} else if strings.HasPrefix(curBase, base) {
			// Ensure suffix is a number ".0001", ".0002", etc.
			test := strings.TrimLeft(strings.TrimPrefix(curBase, base+"."), "0")
			if _, e := strconv.ParseInt(test, 10, 32); e == nil {
				paths = append(paths, curBase)
			}
		}
	}
	sort.Strings(paths)
	if len(renameIfNeeded) > 0 && renameIfNeeded[0] && len(paths) > 0 && paths[0] != base {
		// Old files were removed, renumber files
		for _, p := range paths {
			src := filepath.Join(dirPath, p)
			t1 := filepath.Join(dirPath, fmt.Sprintf("%s-rename", p))
			os.Rename(src, t1)
		}
		for i, p := range paths {
			src := filepath.Join(dirPath, fmt.Sprintf("%s-rename", p))
			t2 := filepath.Join(dirPath, fmt.Sprintf("%s.%04d", base, i))
			if i == 0 {
				t2 = s.indexPath
			}
			os.Rename(src, t2)
		}
		return s.listIndexes()
	}
	return
}

func (s *Indexer) watchInserts() {
	for {
		select {
		case in := <-s.inserts:
			msg, er := s.codec.Marshal(in)
			if er != nil {
				break
			}
			s.flushLock.Lock()
			if s.crtBatch == nil {
				s.crtBatch = s.getWriteIndex().NewBatch()
			}
			var id string
			if provider, ok := msg.(dao.IndexIDProvider); ok {
				id = provider.IndexID()
			} else {
				id = xid.New().String()
			}
			s.crtBatch.Index(id, msg)
			if s.crtBatch.Size() > 5000 {
				s.flush()
			}
			s.flushLock.Unlock()
		case del := <-s.deletes:
			if id, o := del.(string); o {
				s.flushLock.Lock()
				if s.crtBatch == nil {
					s.crtBatch = s.getWriteIndex().NewBatch()
				}
				s.crtBatch.Delete(id)
				if s.crtBatch.Size() > 5000 {
					s.flush()
				}
				s.flushLock.Unlock()
			}
		case <-s.forceFlush:
			s.flushLock.Lock()
			s.flush()
			s.flushLock.Unlock()
		case <-time.After(3 * time.Second):
			s.flushLock.Lock()
			s.flush()
			s.flushLock.Unlock()
		case <-s.insertsDone:
			s.flushLock.Lock()
			s.flush()
			s.flushLock.Unlock()
			s.searchIndex.Close()
			for _, i := range s.indexes {
				i.Close()
			}
			return
		}
	}
}

func (s *Indexer) rotateIfNeeded() {
	if s.indexPath == "" || s.BleveConfig().RotationSize == -1 {
		return
	}
	checkPath := s.indexPath
	if s.cursor > 0 {
		checkPath = fmt.Sprintf("%s.%04d", s.indexPath, s.cursor)
	}
	du, e := indexDiskUsage(checkPath)
	if e != nil {
		fmt.Println("[pydio.grpc.log] Cannot compute disk usage for bleve index", e.Error())
		return
	}
	if du > s.BleveConfig().RotationSize {
		fmt.Println("Rotating "+s.indexPath+" for size ", du)
		// Open a new index
		newPath := fmt.Sprintf("%s.%04d", s.indexPath, len(s.indexes))
		newIndex, er := s.openOneIndex(newPath, s.BleveConfig().MappingName)
		if er != nil {
			fmt.Println("[pydio.grpc.log] Cannot create new bleve index", er.Error())
			return
		}
		s.indexes = append(s.indexes, newIndex)
		s.searchIndex.Add(newIndex)
		s.cursor = len(s.indexes) - 1
	}
}

func (s *Indexer) flush() {
	if s.crtBatch != nil {
		s.getWriteIndex().Batch(s.crtBatch)
		s.rotateIfNeeded()
		s.crtBatch = nil
	}
}

// Resync creates a copy of current index. It has been originally used for switching analyze format from bleve to scorch.
func (s *Indexer) Resync(logger func(string)) error {

	copyDir := filepath.Join(filepath.Dir(s.indexPath), uuid.New())
	e := os.Mkdir(copyDir, 0777)
	if e != nil {
		return e
	}
	defer func() {
		os.RemoveAll(copyDir)
	}()
	copyPath := filepath.Join(copyDir, filepath.Base(s.indexPath))

	dup := &Indexer{
		DAO: s.DAO,
	}
	if UnitTestEnv {
		dup.inserts = make(chan interface{})
	} else {
		dup.inserts = make(chan interface{}, BufferedChanSize)
	}
	er := dup.Open(copyPath)
	if er != nil {
		return er
	}
	logger("Listing Index inside new one")

	q := bleve.NewMatchAllQuery()
	req := bleve.NewSearchRequest(q)
	req.Size = 5000
	page := 0

	for {

		logger(fmt.Sprintf("Reindexing logs from page %d\n", page))
		req.From = page * req.Size
		req.Fields = []string{"*"}
		sr, err := s.searchIndex.Search(req)
		if err != nil {
			fmt.Println(err)
			return err
		}
		for _, hit := range sr.Hits {
			um, e := s.codec.Unmarshal(hit.Fields)
			if e != nil {
				fmt.Println(e)
				continue
			}
			mu, e := s.codec.Marshal(um)
			if e != nil {
				fmt.Println(e)
				continue
			}
			s.inserts <- mu
		}
		if sr.Total <= uint64((page+1)*req.Size) {
			break
		}
		page++

	}

	s.Close()
	dup.Close()
	<-time.After(5 * time.Second) // Make sure original is closed

	logger("Removing old indexes")
	for _, ip := range s.listIndexes() {
		if err := os.RemoveAll(filepath.Join(filepath.Dir(s.indexPath), ip)); err != nil {
			return err
		}
	}
	logger("Moving new indexes")
	for _, ip := range dup.listIndexes() {
		src := filepath.Join(copyDir, ip)
		target := filepath.Join(filepath.Join(filepath.Dir(s.indexPath), ip))
		if err := os.Rename(src, target); err != nil {
			return err
		}
	}
	logger("Restarting new mr")
	if err := s.Open(s.indexPath); err != nil {
		return err
	}
	logger("Resync operation done")
	return nil

}

// Truncate gathers size of existing indexes, starting from last. When max is reached
// it starts deleting all previous indexes.
func (s *Indexer) Truncate(max int64, logger func(string)) error {
	logger("Closing log server, waiting for five seconds")
	dir := filepath.Dir(s.indexPath)
	s.Close()
	<-time.After(5 * time.Second)
	logger("Start purging old files")
	indexes := s.listIndexes()
	var i int
	var total int64
	var remove bool
	for i = len(indexes) - 1; i >= 0; i-- {
		if remove {
			e := os.RemoveAll(filepath.Join(dir, indexes[i]))
			if e != nil {
				logger(fmt.Sprintf("cannot remove index %s", indexes[i]))
			}
		} else if u, e := indexDiskUsage(filepath.Join(dir, indexes[i])); e == nil {
			total += u
			remove = total > max
		}
	}
	// Now restart - it will renumber files
	logger("Re-opening log server")
	s.Open(s.indexPath)
	logger("Truncate operation done")
	return nil
}

// openOneIndex tries to open an existing index at a given path, or creates a new one
func (s *Indexer) openOneIndex(bleveIndexPath string, mappingName string) (bleve.Index, error) {

	index, err := bleve.Open(bleveIndexPath)
	if err != nil {
		indexMapping := bleve.NewIndexMapping()
		if model, ok := s.codec.GetModel(s.serviceConfigs); ok {
			if docMapping, ok := model.(*mapping.DocumentMapping); ok {
				indexMapping.AddDocumentMapping(mappingName, docMapping)
			}
		}
		// Creates the new index and initializes the server
		if bleveIndexPath == "" {
			index, err = bleve.NewMemOnly(indexMapping)
		} else {
			index, err = bleve.NewUsing(bleveIndexPath, indexMapping, scorch.Name, boltdb.Name, nil)
		}
		if err != nil {
			return nil, err
		}
	}
	return index, nil

}

// indexDiskUsage is a simple implementation for computing directory size
func indexDiskUsage(currPath string) (int64, error) {
	var size int64

	dir, err := os.Open(currPath)
	if err != nil {
		return 0, err
	}
	defer dir.Close()

	files, err := dir.Readdir(-1)
	if err != nil {
		return 0, err
	}

	for _, file := range files {
		if file.IsDir() {
			s, e := indexDiskUsage(filepath.Join(currPath, file.Name()))
			if e != nil {
				return 0, e
			}
			size += s
		} else {
			size += file.Size()
		}
	}

	return size, nil
}