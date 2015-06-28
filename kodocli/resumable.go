package kodocli

import (
	"errors"
	"io"
	"os"
	"sync"

	"qiniupkg.com/x/xlog.v7"

	. "golang.org/x/net/context"
)

// ----------------------------------------------------------

var (
	ErrInvalidPutProgress = errors.New("invalid put progress")
	ErrPutFailed          = errors.New("resumable put failed")
	ErrUnmatchedChecksum  = errors.New("unmatched checksum")
)

const (
	InvalidCtx = 701 // UP: 无效的上下文(bput)，可能情况：Ctx非法或者已经被淘汰（太久未使用）
)

const (
	defaultWorkers   = 4
	defaultChunkSize = 256 * 1024 // 256k
	defaultTryTimes  = 3
)

type Settings struct {
	TaskQsize int // 可选。任务队列大小。为 0 表示取 Workers * 4。
	Workers   int // 并行 Goroutine 数目。
	ChunkSize int // 默认的Chunk大小，不设定则为256k
	TryTimes  int // 默认的尝试次数，不设定则为3
}

var settings = Settings{
	TaskQsize: defaultWorkers * 4,
	Workers:   defaultWorkers,
	ChunkSize: defaultChunkSize,
	TryTimes:  defaultTryTimes,
}

func SetSettings(v *Settings) {

	settings = *v
	if settings.Workers == 0 {
		settings.Workers = defaultWorkers
	}
	if settings.TaskQsize == 0 {
		settings.TaskQsize = settings.Workers * 4
	}
	if settings.ChunkSize == 0 {
		settings.ChunkSize = defaultChunkSize
	}
	if settings.TryTimes == 0 {
		settings.TryTimes = defaultTryTimes
	}
}

// ----------------------------------------------------------

var tasks chan func()

func worker(tasks chan func()) {
	for {
		task := <-tasks
		task()
	}
}

func initWorkers() {

	tasks = make(chan func(), settings.TaskQsize)
	for i := 0; i < settings.Workers; i++ {
		go worker(tasks)
	}
}

func notifyNil(blkIdx int, blkSize int, ret *BlkputRet) {}
func notifyErrNil(blkIdx int, blkSize int, err error)   {}

// ----------------------------------------------------------

const (
	blockBits = 22
	blockMask = (1 << blockBits) - 1
)

func BlockCount(fsize int64) int {
	return int((fsize + blockMask) >> blockBits)
}

// ----------------------------------------------------------

type BlkputRet struct {
	Ctx      string `json:"ctx"`
	Checksum string `json:"checksum"`
	Crc32    uint32 `json:"crc32"`
	Offset   uint32 `json:"offset"`
	Host     string `json:"host"`
}

type RputExtra struct {
	Params     map[string]string                             // 可选。用户自定义参数，以"x:"开头 否则忽略
	MimeType   string                                        // 可选。
	ChunkSize  int                                           // 可选。每次上传的Chunk大小
	TryTimes   int                                           // 可选。尝试次数
	Progresses []BlkputRet                                   // 可选。上传进度
	Notify     func(blkIdx int, blkSize int, ret *BlkputRet) // 可选。进度提示（注意多个block是并行传输的）
	NotifyErr  func(blkIdx int, blkSize int, err error)
}

var once sync.Once

// ----------------------------------------------------------

func (p Uploader) Rput(
	ctx Context, ret interface{}, uptoken string,
	key string, f io.ReaderAt, fsize int64, extra *RputExtra) error {

	return p.rput(ctx, ret, uptoken, key, true, f, fsize, extra)
}

func (p Uploader) RputWithoutKey(
	ctx Context, ret interface{}, uptoken string, f io.ReaderAt, fsize int64, extra *RputExtra) error {

	return p.rput(ctx, ret, uptoken, "", false, f, fsize, extra)
}

func (p Uploader) RputFile(
	ctx Context, ret interface{}, uptoken, key, localFile string, extra *RputExtra) (err error) {

	return p.rputFile(ctx, ret, uptoken, key, true, localFile, extra)
}

func (p Uploader) RputFileWithoutKey(
	ctx Context, ret interface{}, uptoken, localFile string, extra *RputExtra) (err error) {

	return p.rputFile(ctx, ret, uptoken, "", false, localFile, extra)
}

// ----------------------------------------------------------

func (p Uploader) rput(
	ctx Context, ret interface{}, uptoken string,
	key string, hasKey bool, f io.ReaderAt, fsize int64, extra *RputExtra) error {

	once.Do(initWorkers)

	log := xlog.NewWith(ctx)
	blockCnt := BlockCount(fsize)

	if extra == nil {
		extra = new(RputExtra)
	}
	if extra.Progresses == nil {
		extra.Progresses = make([]BlkputRet, blockCnt)
	} else if len(extra.Progresses) != blockCnt {
		return ErrInvalidPutProgress
	}

	if extra.ChunkSize == 0 {
		extra.ChunkSize = settings.ChunkSize
	}
	if extra.TryTimes == 0 {
		extra.TryTimes = settings.TryTimes
	}
	if extra.Notify == nil {
		extra.Notify = notifyNil
	}
	if extra.NotifyErr == nil {
		extra.NotifyErr = notifyErrNil
	}

	var wg sync.WaitGroup
	wg.Add(blockCnt)

	last := blockCnt - 1
	blkSize := 1 << blockBits
	nfails := 0
	p.Conn.Client = newUptokenClient(uptoken, p.Conn.Transport)

	for i := 0; i < blockCnt; i++ {
		blkIdx := i
		blkSize1 := blkSize
		if i == last {
			offbase := int64(blkIdx) << blockBits
			blkSize1 = int(fsize - offbase)
		}
		task := func() {
			defer wg.Done()
			tryTimes := extra.TryTimes
lzRetry:	err := p.resumableBput(ctx, &extra.Progresses[blkIdx], f, blkIdx, blkSize1, extra)
			if err != nil {
				if tryTimes > 1 {
					tryTimes--
					log.Info("resumable.Put retrying ...")
					goto lzRetry
				}
				log.Warn("resumable.Put", blkIdx, "failed:", err)
				extra.NotifyErr(blkIdx, blkSize1, err)
				nfails++
			}
		}
		tasks <- task
	}

	wg.Wait()
	if nfails != 0 {
		return ErrPutFailed
	}

	return p.mkfile(ctx, ret, key, hasKey, fsize, extra)
}

func (p Uploader) rputFile(
	ctx Context, ret interface{}, uptoken string,
	key string, hasKey bool, localFile string, extra *RputExtra) (err error) {

	f, err := os.Open(localFile)
	if err != nil {
		return
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return
	}

	return p.rput(ctx, ret, uptoken, key, hasKey, f, fi.Size(), extra)
}

// ----------------------------------------------------------