package engine

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"Gom3u8/utils/common"
)

const (
	tsExt = ".ts"
	tsFolderName = "ts"
	mergeTSFilename = "main.ts"
	tsTempFileSuffix = "_tmp"
	progressWidth = 40
)

type Downloader struct {
	lock sync.Mutex
	queue []int
	folder string
	tsFolder string
	finish int32
	segLen int
	result *common.Result
}

func NewTask(output string, url string) (*Downloader, error) {
	result, err := common.FromURL(url)
	if err != nil {
		return nil, err
	}
	var folder string
	if output == "" {
		current, err := currentDir()
		if err != nil {
			return nil, err
		}
		folder = filepath.Join(current, output)
	} else {
		folder = output
	}
	if err := os.MkdirAll(folder, os.ModePerm); err != nil {
		return nil, fmt.Errorf("create storage folder failed: %s", err.Error())
	}
	tsFolder := filepath.Join(folder, tsFolderName)
	if err := os.MkdirAll(tsFolder, os.ModePerm); err != nil {
		return nil, fmt.Errorf("create ts folder '[%s]' failed: %s", tsFolder, err.Error())
	}
	d := &Downloader{
		folder: folder, tsFolder: tsFolder, result: result,
	}
	d.segLen = len(result.M3u8.Segments)
	d.queue = genSlice(d.segLen)
	return d, nil
}

func (d *Downloader) Start(concurrency int) error {
	var wg sync.WaitGroup
	limitChan := make(chan struct{}, concurrency)
	for {
		tsIdx, end, err := d.next()
		if err != nil {
			if end {
				break
			}
			continue
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if err := d.download(idx); err != nil {
				fmt.Printf("[failed] %s\n", err.Error())
				if err := d.back(idx); err != nil {
					fmt.Printf(err.Error())
				}
			}
			<-limitChan
		}(tsIdx)
		limitChan <- struct{}{}
	}
	wg.Wait()
	if err := d.merge(); err != nil {
		return err
	}
	return nil
}

func (d *Downloader) download(segIndex int) error {
	tsFilename := tsFilename(segIndex)
	tsUrl := d.tsURL(segIndex)
	b, e := common.Get(tsUrl)
	if e != nil {
		return fmt.Errorf("request %s, %s", tsUrl, e.Error())
	}
	defer b.Close()
	fPath := filepath.Join(d.tsFolder, tsFilename)
	fTemp := fPath + tsTempFileSuffix
	f, err := os.Create(fTemp)
	if err != nil {
		return fmt.Errorf("create file: %s, %s", tsFilename, err.Error())
	}
	bytes, err := ioutil.ReadAll(b)
	if err != nil {
		return fmt.Errorf("read bytes: %s, %s", tsUrl, err.Error())
	}
	sf := d.result.M3u8.Segments[segIndex]
	if sf == nil {
		return fmt.Errorf("invalid segment index: %d", segIndex)
	}
	key, ok := d.result.Keys[sf.KeyIndex]
	if ok && key != "" {
		bytes, err = common.AES128Decrypt(bytes, []byte(key),
			[]byte(d.result.M3u8.Keys[sf.KeyIndex].IV))
		if err != nil {
			return fmt.Errorf("decryt: %s, %s", tsUrl, err.Error())
		}
	}
	syncByte := uint8(71)
	bLen := len(bytes)
	for j := 0; j < bLen; j++ {
		if bytes[j] == syncByte {
			bytes = bytes[j:]
			break
		}
	}
	w := bufio.NewWriter(f)
	if _, err := w.Write(bytes); err != nil {
		return fmt.Errorf("write to %s: %s", fTemp, err.Error())
	}
	_ = f.Close()
	if err = os.Rename(fTemp, fPath); err != nil {
		return err
	}
	atomic.AddInt32(&d.finish, 1)
	fmt.Printf("[download %6.2f%%] %s\n", float32(d.finish)/float32(d.segLen)*100, tsUrl)
	return nil
}

func (d *Downloader) next() (segIndex int, end bool, err error) {
	d.lock.Lock()
	defer d.lock.Unlock()
	if len(d.queue) == 0 {
		err = fmt.Errorf("queue empty")
		if d.finish == int32(d.segLen) {
			end = true
			return
		}
		end = false
		return
	}
	segIndex = d.queue[0]
	d.queue = d.queue[1:]
	return
}

func (d *Downloader) back(segIndex int) error {
	d.lock.Lock()
	defer d.lock.Unlock()
	if sf := d.result.M3u8.Segments[segIndex]; sf == nil {
		return fmt.Errorf("invalid segment index: %d", segIndex)
	}
	d.queue = append(d.queue, segIndex)
	return nil
}

func (d *Downloader) merge() error {
	missingCount := 0
	for idx := 0; idx < d.segLen; idx++ {
		tsFilename := tsFilename(idx)
		f := filepath.Join(d.tsFolder, tsFilename)
		if _, err := os.Stat(f); err != nil {
			missingCount++
		}
	}
	if missingCount > 0 {
		fmt.Printf("[warning] %d files missing\n", missingCount)
	}

	mFilePath := filepath.Join(d.folder, mergeTSFilename)
	mFile, err := os.Create(mFilePath)
	if err != nil {
		return fmt.Errorf("create main TS file failed：%s", err.Error())
	}
	defer mFile.Close()

	writer := bufio.NewWriter(mFile)
	mergedCount := 0
	for segIndex := 0; segIndex < d.segLen; segIndex++ {
		tsFilename := tsFilename(segIndex)
		bytes, err := ioutil.ReadFile(filepath.Join(d.tsFolder, tsFilename))
		_, err = writer.Write(bytes)
		if err != nil {
			continue
		}
		mergedCount++
		drawProgressBar("merge", float32(mergedCount)/float32(d.segLen), progressWidth)
	}
	_ = writer.Flush()
	_ = os.RemoveAll(d.tsFolder)

	if mergedCount != d.segLen {
		fmt.Printf("[warning] \n%d files merge failed", d.segLen-mergedCount)
	}
	fmt.Printf("\n[output] %s\n", mFilePath)
	return nil
}

func (d *Downloader) tsURL(segIndex int) string {
	seg := d.result.M3u8.Segments[segIndex]
	return common.ResolveURL(d.result.URL, seg.URI)
}

func tsFilename(ts int) string {
	return strconv.Itoa(ts) + tsExt
}

func genSlice(len int) []int {
	s := make([]int, 0)
	for i := 0; i < len; i++ {
		s = append(s, i)
	}
	return s
}

func currentDir(joinPath ...string) (string, error) {
	dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		return "", err
	}
	p := strings.Replace(dir, "\\", "/", -1)
	whole := filepath.Join(joinPath...)
	whole = filepath.Join(p, whole)
	return whole, nil
}

func drawProgressBar(prefix string, proportion float32, width int, suffix ...string) {
	pos := int(proportion * float32(width))
	s := fmt.Sprintf("[%s] %s%*s %6.2f%% %s",
		prefix, strings.Repeat("■", pos), width-pos, "", proportion*100, strings.Join(suffix, ""))
	fmt.Print("\r" + s)
}
