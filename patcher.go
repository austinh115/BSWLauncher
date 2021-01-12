package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"github.com/dustin/go-humanize"
	"github.com/klauspost/compress/s2"
	"github.com/vbauerster/mpb"
	"github.com/vbauerster/mpb/decor"
	"golang.org/x/crypto/blake2b"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const NumCDNs = 5
const DefaultForceDownload = false

var installDirectory, _ = os.Getwd()
var progressBarManager = mpb.New()
var onlineCDNs []int
var onlineServers int

type File struct {
	PathLen      uint32
	Path         string
	HashLen      uint32
	Hash         string
	LastModified int64
}

type VersionFile struct {
	Padding       [16]byte
	NumberOfFiles uint32
	Files         []File
}

func main() {
	if onlineCDNs, onlineServers = checkCDNStatus(); onlineServers == 0 {
		log.Fatal("There are no download servers online. Message the BSW admins if there is no post in #news already.")
	}

	versionFile, err := fetchVersionFile()
	if err != nil {
		log.Fatal("Could not fetch the version file.")
	}
	fmt.Printf("Fetched version information for %v files.\n", versionFile.NumberOfFiles)

	toDownload := verifyFiles(versionFile.Files)
	fmt.Printf("Found %v files that need to be updated.\n", len(toDownload))

	downloadFiles(toDownload, runtime.NumCPU())
}

func checkCDNStatus() ([]int, int) {
	var online []int
	for i := 0; i < NumCDNs; i++ {
		formattedUrl := fmt.Sprintf("https://cdn%v.burningsw.to/", i)
		resp, err := http.Head(formattedUrl)
		if err == nil {
			if resp.StatusCode == 200 {
				online = append(online, i)
			}
		}
	}
	return online, len(online)
}

func fetchVersionFile() (*VersionFile, error) {
	data, err := getFile(fmt.Sprintf("https://cdn%v.burningsw.to/version.bin", onlineCDNs[0]))
	if err != nil {
		return nil, err
	}

	for i := range data {
		data[i] ^= byte(i%0xFF + 0x69)
	}
	buffer := bytes.NewBuffer(data)

	versionFile := &VersionFile{}

	_ = binary.Read(buffer, binary.LittleEndian, &versionFile.Padding)

	_ = binary.Read(buffer, binary.LittleEndian, &versionFile.NumberOfFiles)
	versionFile.Files = make([]File, versionFile.NumberOfFiles)

	for i := range versionFile.Files {
		file := &versionFile.Files[i]

		_ = binary.Read(buffer, binary.LittleEndian, &file.PathLen)
		strBuffer := make([]byte, file.PathLen)

		_ = binary.Read(buffer, binary.LittleEndian, &strBuffer)
		file.Path = string(strBuffer)

		_ = binary.Read(buffer, binary.LittleEndian, &file.HashLen)
		hashBuffer := make([]byte, file.HashLen)

		_ = binary.Read(buffer, binary.LittleEndian, &hashBuffer)
		file.Hash = string(hashBuffer)

		_ = binary.Read(buffer, binary.LittleEndian, &file.LastModified)
	}

	return versionFile, nil
}

func verifyFiles(files []File) []File {
	var toDownload []File

	for _, file := range files {
		fileName := file.Path

		hasher, _ := blake2b.New256(nil)
		fmt.Print("Checking ", fileName, ": ")
		localFile, err := os.Open(filepath.Join(installDirectory, fileName))

		if err != nil {
			println("Need to download.")
			toDownload = append(toDownload, file)
			continue
		}
		fi, err := localFile.Stat()
		if err == nil && fi.Mode() == os.FileMode(0444) {
			println("File is custom (read-only), skipping.")
			continue
		}
		if _, err := io.Copy(hasher, localFile); err != nil {
			println("Need to download.")
			toDownload = append(toDownload, file)
			continue
		}
		_ = localFile.Close()

		hashBytes := hasher.Sum(nil)
		hash := hex.EncodeToString(hashBytes[:])

		if hash != file.Hash {
			println("Need to download.")
			toDownload = append(toDownload, file)
		} else {
			lm := time.Unix(file.LastModified, 0)
			err = os.Chtimes(file.Path, lm, lm)
			println("OK.")
		}
	}

	return toDownload
}

func downloadFiles(toDownload []File, numWorkers int) {
	var wg sync.WaitGroup
	wg.Add(len(toDownload))

	jobs := make(chan File, len(toDownload))

	for w := 0; w < numWorkers; w++ {
		go worker(w, jobs, &wg)
	}

	for _, file := range toDownload {
		jobs <- file
	}

	defer close(jobs)

	wg.Wait()
}

func worker(id int, jobs <-chan File, wg *sync.WaitGroup) {
	for j := range jobs {
		formattedUrl := fmt.Sprintf("https://cdn%v.burningsw.to/%s", onlineCDNs[id%onlineServers], j.Path)
		formattedUrl = strings.ReplaceAll(formattedUrl, "\\", "/")
		force := DefaultForceDownload
		for {
			err := downloadFile(j, formattedUrl, wg, force)
			if err != nil {
				if force {
					println("Download for", formattedUrl, "failed again, check manually.")
					wg.Done()
					break
				}
				// force download fresh
				log.Print(err)
				println(" (" + formattedUrl + "), Retrying")
				force = true
				continue
			}
			break
		}
	}
}

func downloadFile(file File, url string, wg *sync.WaitGroup, force bool) error {
	filename := file.Path
	// Create the file, but give it a tmp file extension, this means we won't overwrite a
	// file until it's downloaded, but we'll remove the tmp extension once downloaded.
	info, err := os.Stat(filename + ".tmp")

	var currPosition int64
	var out *os.File
	x, dlerr := http.NewRequest("GET", url, nil)
	if dlerr != nil {
		log.Fatal(dlerr)
		return err
	}

	if !force && err == nil {
		currPosition = info.Size()
		/* DownloadNewestFileCheck
		if info.ModTime().Unix() != file.LastModified { // Doesn't work because golang changes file modtime on write
			fmt.Printf("File %s modification time has changed, assuming new version (%v vs %v)\n", info.Name(), info.ModTime().Format("1/2/2006 3:04 PM"), time.Unix(file.LastModified, 0).Format("1/2/2006 3:04 PM"))
			currPosition = 0
		} else {
		*/
		x.Header.Add("Range", fmt.Sprintf("bytes=%v-", currPosition))
		fmt.Printf("Resuming %s from byte position %v.\n", filename, humanize.Bytes(uint64(currPosition)))
		//}
		out, err = os.OpenFile(filename+".tmp", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0777)
		if err != nil {
			return err
		}
	} else {
		out, err = os.Create(filename + ".tmp")
	}

	// Get the data
	client := &http.Client{}
	resp, err := client.Do(x)
	if err != nil {
		return err
	}

	// Create our progress reporter and pass it to be used alongside our writer
	bar := progressBarManager.AddBar(currPosition+resp.ContentLength,
		mpb.PrependDecorators(
			decor.Name(filename+" > "),
			decor.CountersKibiByte("% .2f / % .2f"),
		),
		mpb.AppendDecorators(
			decor.OnComplete(
				decor.EwmaETA(decor.ET_STYLE_GO, 60),
				"done",
			),
			decor.Name(" @ "),
			decor.EwmaSpeed(decor.UnitKiB, "% .2f", 60),
		),
	)

	defer bar.Abort(true) // Remove the bar when it's done downloading to clean up the console
	proxyReader := bar.ProxyReader(resp.Body)
	if currPosition > 0 {
		bar.IncrInt64(currPosition)
	}
	if _, err = io.Copy(out, proxyReader); err != nil {
		//log.Fatal(err)
		return err
	}
	defer proxyReader.Close() // Close file handles on exit
	defer resp.Body.Close()

	decompress, err := os.Create(filename)
	if err != nil {
		//log.Fatal(err)
		return err
	}

	_ = out.Close()
	out, err = os.Open(filename + ".tmp") // reopen for reading
	if err != nil {
		//log.Fatal(err)
		return err
	}

	if _, err = io.Copy(decompress, s2.NewReader(out)); err != nil { // Decompress the data using s2d
		return err
	}
	_ = out.Close()
	_ = os.Remove(filename + ".tmp")

	lm := time.Unix(file.LastModified, 0)
	err = os.Chtimes(file.Path, lm, lm)

	wg.Done()
	return nil
}

func getFile(path string) ([]byte, error) {
	resp, err := http.Get(path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	return body, err
}
