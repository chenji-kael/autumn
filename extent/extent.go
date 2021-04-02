/*
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless  by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package extent

import (
	"io"
	"io/ioutil"

	"github.com/pkg/errors"

	"bytes"
	"encoding/json"
	"math"
	"os"
	"sync/atomic"

	"github.com/journeymidnight/autumn/extent/record"
	"github.com/journeymidnight/autumn/proto/pb"
	"github.com/journeymidnight/autumn/rangepartition/y"
	"github.com/journeymidnight/autumn/utils"
	"github.com/journeymidnight/autumn/xlog"
	"github.com/pkg/xattr"
)

const (
	extentMagicNumber = "EXTENTXX"
	XATTRMETA         = "user.EXTENTMETA"
	XATTRSEAL         = "user.XATTRSEAL"
)

var (
	EndOfExtent = errors.New("EndOfExtent")
	EndOfStream = errors.New("EndOfStream")
)

type Extent struct {
	//sync.Mutex //only one AppendBlocks could be called at a time
	utils.SafeMutex
	isSeal       int32  //atomic
	commitLength uint32 //atomic
	ID           uint64
	fileName     string
	file         *os.File
	//FIXME: add SSD Chanel
	writer *record.LogWriter
}

//format to JSON
type extentHeader struct {
	MagicNumber []byte
	ID          uint64
	kBlockSize  int
}

func (eh *extentHeader) Marshal() []byte {
	data, err := json.Marshal(eh)
	utils.Check(err)
	return data
}

func (eh *extentHeader) Unmarshal(data []byte) error {
	return json.Unmarshal(data, eh)
}

func newExtentHeader(ID uint64) *extentHeader {
	eh := extentHeader{
		ID: ID,
	}
	eh.MagicNumber = []byte(extentMagicNumber)
	return &eh
}

func readExtentHeader(file *os.File) (*extentHeader, error) {

	d, err := xattr.FGet(file, XATTRMETA)
	if err != nil {
		return nil, err
	}

	eh := newExtentHeader(0)

	if err = eh.Unmarshal(d); err != nil {
		return nil, err
	}

	if eh.ID == 0 || bytes.Compare(eh.MagicNumber, []byte(extentMagicNumber)) != 0 {
		return nil, errors.New("meta data is not corret")
	}
	return eh, nil

}

func CreateExtent(fileName string, ID uint64) (*Extent, error) {
	f, err := os.OpenFile(fileName, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	extentHeader := newExtentHeader(ID)
	value := extentHeader.Marshal()

	if err := xattr.FSet(f, XATTRMETA, value); err != nil {
		return nil, err
	}

	f.Sync()

	//write header of Extent
	return &Extent{
		ID:           ID,
		isSeal:       0,
		commitLength: 0,
		fileName:     fileName,
		file:         f,
		writer:       record.NewLogWriter(f, 0, 0),
	}, nil

}

func OpenExtent(fileName string) (*Extent, error) {

	d, err := xattr.LGet(fileName, XATTRSEAL)

	//if extent is a sealed extent
	if err == nil && bytes.Compare(d, []byte("true")) == 0 {
		file, err := os.Open(fileName)
		if err != nil {
			return nil, err
		}
		info, _ := file.Stat()
		if info.Size() > math.MaxUint32 {
			return nil, errors.Errorf("check extent file, the extent file is too big")
		}
		//check extent header

		eh, err := readExtentHeader(file)
		if err != nil {
			return nil, err
		}

		return &Extent{
			isSeal:       1,
			commitLength: uint32(info.Size()),
			fileName:     fileName,
			file:         file,
			ID:           eh.ID,
		}, nil
	}

	/*
		如果extent是意外关闭
		1. 他的3副本很可能不一致. 如果有新的写入, 在primary上面是Append, 在secondary上面的API
		是检查Offset的Append, 如果这3个任何一个失败, client就找sm把extent变成:truncate/Sealed.
		2. 由于写入是多个sector(record.BlockSize至少32KB),也会有一致性问题:
		log的格式是n * record.BlockSize + tail, 一般不一致是在tail部分, 和leveldb类似, 在replayWAL
		时, 如果发现错误, 则create新的extent或者总是create新extent(rocksdb或者leveldb逻辑)
	*/

	//前面可以只读block的meta, 直到最后一个block再读文件数据, 检查checksum
	f, err := os.OpenFile(fileName, os.O_APPEND|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	info, _ := f.Stat()
	currentSize := uint32(info.Size())

	eh, err := readExtentHeader(f)
	if err != nil {
		return nil, err
	}

	bn := (info.Size() / record.BlockSize)
	offset := int32(info.Size()) % record.BlockSize

	return &Extent{
		isSeal:       0,
		commitLength: currentSize,
		fileName:     fileName,
		file:         f,
		ID:           eh.ID,
		writer:       record.NewLogWriter(f, bn, offset),
	}, nil
}

//support multple threads
//limit max read size
type extentReader struct {
	extent *Extent
	pos    int64
}

func (r *extentReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekEnd:
		r.pos += offset //offset is nagative
	case io.SeekStart:
		r.pos = offset
	case io.SeekCurrent:
		r.pos += offset
	default:
		return 0, errors.New("not supported")
	}
	return int64(r.pos), nil

}

//readfull
func (r *extentReader) Read(p []byte) (n int, err error) {
	n, err = r.extent.file.ReadAt(p, r.pos)
	if err != nil {
		return n, err
	}
	r.pos += int64(n)
	return n, nil
}

func (ex *Extent) Seal(commit uint32) error {
	ex.Lock()
	defer ex.Unlock()
	atomic.StoreInt32(&ex.isSeal, 1)

	currentLength := ex.commitLength
	if currentLength < commit {
		return errors.Errorf("commit is less than current commit length")
	} else if currentLength > commit {
		ex.file.Truncate(int64(commit))
	}

	if err := xattr.FSet(ex.file, XATTRSEAL, []byte("true")); err != nil {
		return err
	}

	return nil

}

func (ex *Extent) IsSeal() bool {
	return atomic.LoadInt32(&ex.isSeal) == 1
}

func (ex *Extent) getReader(offset uint32) *extentReader {
	return &extentReader{
		extent: ex,
		pos:    int64(offset),
	}

}

func (ex *Extent) Close() {
	ex.Lock()
	defer ex.Unlock()
	ex.file.Close()
}

func (ex *Extent) AppendBlocks(blocks []*pb.Block, lastCommit *uint32, doSync bool) ([]uint32, uint32, error) {
	ex.AssertLock()

	if atomic.LoadInt32(&ex.isSeal) == 1 {
		return nil, 0, errors.Errorf("immuatble")
	}

	//for secondary extents, it must check lastCommit.
	if lastCommit != nil && *lastCommit != ex.CommitLength() {
		return nil, 0, errors.Errorf("offset not match...")
	}

	currentLength := atomic.LoadUint32(&ex.commitLength)

	truncate := func() {
		ex.file.Truncate(int64(currentLength))
		ex.commitLength = currentLength
	}

	var offsets []uint32
	for _, block := range blocks {
		start, end, err := ex.writer.WriteRecord(block.Data)
		if err != nil {
			defer truncate()
			return nil, 0, err
		}
		//if we have ssd journal, do not have to sync every time.
		//TODO: wait ssd channel,  这里分情况, 如果有SSD journal, 就不需要调用sync
		//如果没有SSD journal,就需要调用sync
		offsets = append(offsets, uint32(start))
		currentLength = uint32(end)
	}
	ex.writer.Flush()
	if doSync {
		//FIXME
		ex.file.Sync()
	}

	atomic.StoreUint32(&ex.commitLength, currentLength)
	return offsets, currentLength, nil
}

func (ex *Extent) ReadBlocks(offset uint32, maxNumOfBlocks uint32, maxTotalSize uint32) ([]*pb.Block, []uint32, uint32, error) {

	var ret []*pb.Block
	//TODO: fix block number
	current := atomic.LoadUint32(&ex.commitLength)
	if current <= offset {
		if ex.IsSeal() {
			return nil, nil, 0, EndOfExtent
		} else {
			return nil, nil, 0, EndOfStream
		}
	}

	wrapReader := ex.getReader(0) //thread-safe
	rr := record.NewReader(wrapReader)
	err := rr.SeekRecord(int64(offset))
	if err != nil {
		return nil, nil, 0, err
	}
	size := uint32(0)

	var offsets []uint32
	for i := uint32(0); i < maxNumOfBlocks; i++ {

		start := wrapReader.pos
		reader, err := rr.Next()
		if err == io.EOF {
			if ex.IsSeal() {
				return ret, offsets, uint32(wrapReader.pos), EndOfExtent
			} else {
				return ret, offsets, uint32(wrapReader.pos), EndOfStream
			}
		}

		if err != nil {
			//TODO: we can call rr.Recover() to continue
			return nil, nil, 0, err
		}

		data, err := ioutil.ReadAll(reader)

		size += uint32(len(data))
		if size > maxTotalSize {
			break
		}
		ret = append(ret, &pb.Block{data})
		offsets = append(offsets, uint32(start))
	}
	return ret, offsets, uint32(wrapReader.pos), nil
}

func (ex *Extent) CommitLength() uint32 {
	return atomic.LoadUint32(&ex.commitLength)
}

//helper function, block could be pb.Entries, support ReadEntries
func (ex *Extent) ReadEntries(offset uint32, maxTotalSize uint32, replay bool) ([]*pb.EntryInfo, uint32, error) {
	blocks, offsets, end, err := ex.ReadBlocks(offset, 10, maxTotalSize)
	if err != nil && err != EndOfStream && err != EndOfExtent {
		return nil, 0, err
	}
	var ret []*pb.EntryInfo
	for i := range blocks {
		e, err := ExtractEntryInfo(blocks[i], ex.ID, offsets[i], replay)
		if err != nil {
			xlog.Logger.Error(err)
			continue

		}
		ret = append(ret, e)
	}

	return ret, end, err
}

func ExtractEntryInfo(b *pb.Block, extentID uint64, offset uint32, replay bool) (*pb.EntryInfo, error) {
	entry := new(pb.Entry)
	if err := entry.Unmarshal(b.Data); err != nil {
		return nil, err
	}

	if y.ShouldWriteValueToLSM(entry) {
		if replay { //replay read
			return &pb.EntryInfo{
				Log:           entry,
				EstimatedSize: uint64(entry.Size()),
				ExtentID:      extentID,
				Offset:        offset,
			}, nil
		} else { //gc read
			entry.Value = nil //或者可以直接返回空
			return &pb.EntryInfo{
				Log:           entry,
				EstimatedSize: uint64(entry.Size()),
				ExtentID:      extentID,

				Offset: offset,
			}, nil
		}
	} else {
		//big value
		//keep entry.Value and make sure BitValuePointer
		entry.Meta |= uint32(y.BitValuePointer)
		//set value to nil to save network bandwidth
		entry.Value = nil
		return &pb.EntryInfo{
			Log:           entry,
			EstimatedSize: uint64(entry.Size()),
			ExtentID:      extentID,
			Offset:        offset,
		}, nil
	}
}
