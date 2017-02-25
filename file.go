package gobuddyfs

import (
	"encoding/binary"

	"bytes"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"github.com/golang/glog"
	"golang.org/x/net/context"
)

// TODO: Determine a mechanism to spill over metadata chunks into more block(s).
// For files which are very large, encoded form of "Blocks" may not fit within a
// data block.
//
// Currently, we do not consider this case at all. With a 4KB block size,
// and block list entries being approx 10 bytes long (Block contains a name
// field which is not always relevant), a File metadata block can contain ~408
// block entries, totalling ~1.62MB. With 32K blocks, we can get 102MB. So, this
type File struct {
	Block
	Blocks     []StorageUnit
	Size       uint64
	BlockSize  uint64
	KVS        KVStore              `json:"-"`
	blkGen     BlockGenerator       `json:"-"`
	BlockCache map[int64]*DataBlock `json:"-"`
	BFS        *BuddyFS             `json:"-"`
}

var _ Marshalable = new(File)

func (file *File) Open(ctx context.Context, req *fuse.OpenRequest, res *fuse.OpenResponse) (fs.Handle, error) {
	if glog.V(2) {
		glog.Infoln("Open called")
	}
	return file, nil
}

func (file *File) getBlock(index int64) *DataBlock {
	if glog.V(2) {
		glog.Infoln("GetBlock:", index)
	}

	if uint64(index) >= blkCount(file.Size, BLOCK_SIZE) {
		return nil
	}

	if file.BlockCache == nil {
		file.BlockCache = make(map[int64]*DataBlock)
	}

	blkId := file.Blocks[index].GetId()

	if file.BlockCache[blkId] == nil {
		// TODO: This mechanism of fetching blocks from disk to cache makes testing
		// harder. Find an alternate mechanism to do so.
		startBlock := &DataBlock{StorageUnit: &Block{}}
		startBlock.SetId(blkId)
		err := startBlock.ReadBlock(startBlock, file.KVS)
		if err != nil {
			glog.Errorf("Error while reading data block: %q", err)
			return nil
		}

		file.BlockCache[blkId] = startBlock
	}

	return file.BlockCache[blkId]
}

func (file *File) appendBlock(dblk *DataBlock) {
	if glog.V(2) {
		glog.Infoln("AppendBlock: ", len(file.BlockCache))
	}

	if file.BlockCache == nil {
		file.BlockCache = make(map[int64]*DataBlock)
	}

	file.BlockCache[dblk.GetId()] = dblk
	file.MarkDirty()
}

func blkCount(size uint64, BLK_SIZE uint64) uint64 {
	return (size + BLK_SIZE - 1) / BLK_SIZE
}

// TODO: Should the return type be a standard error instead?
// TODO: Unit tests!
func (file *File) setSize(size uint64) error {
	newBlockCount := blkCount(size, BLOCK_SIZE)

	if newBlockCount < uint64(len(file.Blocks)) {
		if glog.V(2) {
			glog.Infoln("Reducing number of blocks to", newBlockCount)
		}
		blocksToDelete := file.Blocks[newBlockCount:]
		file.Blocks = file.Blocks[:newBlockCount]

		for blk := range blocksToDelete {
			delete(file.BlockCache, blocksToDelete[blk].GetId())
			blocksToDelete[blk].Delete(file.KVS)
		}
	} else if newBlockCount > uint64(len(file.Blocks)) {
		if glog.V(2) {
			glog.Infoln("Increasing number of blocks to", newBlockCount)
		}
		for uint64(len(file.Blocks)) < newBlockCount {
			blk := file.blkGen.NewBlock()
			dBlk := DataBlock{StorageUnit: blk, Data: []byte{}}
			dBlk.MarkDirty()

			file.Blocks = append(file.Blocks, blk)
			file.appendBlock(&dBlk)
		}
	}

	// Else, the file size change did not change the number of blocks.
	// Essentially, the change was limited to the last block. In this case, we
	// simply change the size attribute and proceed.

	file.Size = size
	file.MarkDirty()

	return nil
}

func (file *File) Setattr(ctx context.Context, req *fuse.SetattrRequest, res *fuse.SetattrResponse) error {
	if glog.V(2) {
		glog.Infoln("Setattr called")
		glog.Infoln("Req: ", req)
	}

	metaChanges := false
	valid := req.Valid
	if valid.Size() && req.Size != file.Size {
		file.setSize(req.Size)
		metaChanges = true
	}

	currAttr := fuse.Attr{}
	file.Attr(ctx, &currAttr)
	if res.Attr != currAttr {
		res.Attr = currAttr
		metaChanges = true
	}

	// TODO: Handle or ignore metadata changes like uid/gid/timestamps.

	if metaChanges {
		// There are metadata changes to the file, write back before proceeding.
		return file.Flush(ctx, nil)
	}

	return nil
}

func (file *File) Write(ctx context.Context, req *fuse.WriteRequest, res *fuse.WriteResponse) error {
	dataBytes := len(req.Data)
	if glog.V(2) {
		glog.Infof("Writing %d byte(s) at offset %d", dataBytes, req.Offset)
	}

	// In case we write past current EOF, expand the file.
	if uint64(req.Offset)+uint64(dataBytes) > file.Size {
		file.setSize(uint64(req.Offset) + uint64(dataBytes))
	}

	// TODO: Write currently only updates one block worth of data.
	startBlockId := req.Offset / BLOCK_SIZE

	var startBlock *DataBlock = file.getBlock(startBlockId)

	if glog.V(2) {
		glog.Infof("Block content length: %d", len(startBlock.Data))
	}

	bytesToAdd := min(BLOCK_SIZE-int(req.Offset%BLOCK_SIZE), dataBytes)
	data := append(startBlock.Data[:(req.Offset%BLOCK_SIZE)], req.Data[:bytesToAdd]...)
	if len(startBlock.Data) >= int(req.Offset%BLOCK_SIZE)+bytesToAdd {
		data = append(data, startBlock.Data[int(req.Offset%BLOCK_SIZE)+bytesToAdd:]...)
	}
	startBlock.Data = data

	if glog.V(2) {
		glog.Infof("Block content length after: %d", len(startBlock.Data))
	}

	startBlock.MarkDirty()

	if glog.V(2) {
		glog.Infoln("Successfully completed write operation")
	}

	res.Size = bytesToAdd
	file.MarkDirty()
	return nil
}

func (file *File) Marshal() ([]byte, error) {
	var buf = new(bytes.Buffer)

	binary.Write(buf, binary.LittleEndian, &file.Size)
	binary.Write(buf, binary.LittleEndian, file.BlockSize)

	// TODO: This might be redundant given blocksize and file size.
	binary.Write(buf, binary.LittleEndian, int64(len(file.Blocks)))

	for _, blk := range file.Blocks {
		// TODO: This is the right place to do a spill check, to put data into the
		// next block.
		binary.Write(buf, binary.LittleEndian, blk.GetId())
	}

	return buf.Bytes(), nil
}

func (file *File) Unmarshal(data []byte) error {
	var err error
	var sz int64
	rd := bytes.NewReader(data)

	err = binary.Read(rd, binary.LittleEndian, &file.Size)
	if err != nil {
		return err
	}

	err = binary.Read(rd, binary.LittleEndian, &file.BlockSize)
	if err != nil {
		return err
	}

	err = binary.Read(rd, binary.LittleEndian, &sz)
	if err != nil {
		return err
	}

	file.Blocks = make([]StorageUnit, sz)

	for i := 0; i < int(sz); i++ {
		var blkId int64
		err = binary.Read(rd, binary.LittleEndian, &blkId)
		if err != nil {
			return err
		}

		file.Blocks[i] = &Block{Id: blkId}
	}

	file.BlockCache = make(map[int64]*DataBlock)

	return nil
}

func (file File) Attr(ctx context.Context, attr *fuse.Attr) error {
	if glog.V(2) {
		glog.Infoln("Attr called", file.Name)
	}

	attr.Mode = 0444
	attr.Inode = uint64(file.Id)
	attr.Blocks = uint64(len(file.Blocks))
	attr.BlockSize = uint32(file.BlockSize)
	attr.Size = file.Size
	return nil
}

func (file *File) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	if glog.V(2) {
		glog.Infoln("Release", file.Name)
	}
	return nil
}

func (file *File) Forget() {
	if glog.V(2) {
		glog.Infoln("FORGET", file.Name)
	}
}

func (file *File) Flush(ctx context.Context, req *fuse.FlushRequest) error {
	if glog.V(2) {
		glog.Infoln("FLUSH", file.Name, file.IsDirty())
	}
	for i := range file.BlockCache {
		if file.BlockCache[i] != nil && file.BlockCache[i].IsDirty() {
			err := file.BlockCache[i].WriteBlock(file.BlockCache[i], file.KVS)
			if err != nil {
				glog.Warning("Unable to write block %s due to error: %s",
					file.Blocks[i].GetId(), err)
			} else {
				// TODO: Use an LRU cache to keep blocks in memory. Dropping dirty items
				// as soon as they are written is wasteful. On the other hand, not
				// dropping these items will fill up memory and cause OOMs for
				// relatively small sized files.

				file.BlockCache[i].MarkClean()
				// file.BlockCache[i] = nil
			}
		}
	}

	if file.IsDirty() {
		file.WriteBlock(file, file.KVS)
	}
	return nil
}

func (file *File) Fsync(ctx context.Context, req *fuse.FsyncRequest) error {
	if glog.V(2) {
		glog.Infoln("FSYNC", file.Name, file.IsDirty())
	}
	return file.Flush(ctx, nil)
}

func (file *File) Read(ctx context.Context, req *fuse.ReadRequest, res *fuse.ReadResponse) error {
	if glog.V(2) {
		glog.Infof("Reading %d byte(s) at offset %d", req.Size, req.Offset)
	}

	if req.Offset >= int64(file.Size) {
		res.Data = []byte{}
		return nil
	}

	res.Data = []byte{}

	startBlockId := req.Offset / BLOCK_SIZE

	var startBlock *DataBlock = file.getBlock(startBlockId)
	if startBlock == nil {
		glog.Error("Error while reading block")
		return fuse.EIO
	}

	beginReadByte := int(req.Offset % BLOCK_SIZE)
	endReadByte := min(len(startBlock.Data)-beginReadByte, req.Size)
	if glog.V(2) {
		glog.Infof("Block content length: %d", len(startBlock.Data))
		glog.Infof("Reading from %d to %d in block %d", beginReadByte, endReadByte+beginReadByte, startBlockId)
	}
	res.Data = startBlock.Data[beginReadByte : endReadByte+beginReadByte]

	return nil
}
