package benchmark

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/zilliztech/woodpecker/common/config"
	"github.com/zilliztech/woodpecker/woodpecker"
	"github.com/zilliztech/woodpecker/woodpecker/log"
)

func TestE2EWrite(t *testing.T) {
	startGopsAgent()

	testCases := []struct {
		name        string
		storageType string
		rootPath    string
	}{
		{
			name:        "LocalFsStorage",
			storageType: "local",
			rootPath:    "/tmp/TestE2EWrite",
		},
		{
			name:        "ObjectStorage",
			storageType: "", // Use default storage type minio-compatible
			rootPath:    "", // No need to specify path when using default storage
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.NewConfiguration()
			assert.NoError(t, err)

			if tc.storageType != "" {
				cfg.Woodpecker.Storage.Type = tc.storageType
			}
			if tc.rootPath != "" {
				cfg.Woodpecker.Storage.RootPath = tc.rootPath
			}

			client, err := woodpecker.NewEmbedClientFromConfig(context.Background(), cfg)
			assert.NoError(t, err)

			// ###  CreateLog if not exists
			client.GetMetadataProvider().CreateLog(context.Background(), "test_log")

			// ### OpenLog
			logHandle, openErr := client.OpenLog(context.Background(), "test_log")
			assert.NoError(t, openErr)
			logHandle.GetName()

			//	### OpenWriter
			logWriter, openWriterErr := logHandle.OpenLogWriter(context.Background())
			assert.NoError(t, openWriterErr)
			writeResultChan := logWriter.WriteAsync(context.Background(),
				&log.WriterMessage{
					Payload: []byte("hello world 1"),
				},
			)
			writeResult := <-writeResultChan
			assert.NoError(t, writeResult.Err)
			fmt.Printf("write success, returned recordId:%v\n", writeResult.LogMessageId)

			err = logWriter.Close(context.Background())
			assert.NoError(t, err)
			err = client.Close()
			assert.NoError(t, err)
		})
	}
}

func TestAsyncWriteThroughput(t *testing.T) {
	startGopsAgent()
	startMetrics()
	startReporting()
	entrySize := 1_000_000 // 1MB per row
	batchCount := 1_000    // wait for batch entries to finish
	writeCount := 10_000   // total rows to write

	testCases := []struct {
		name        string
		storageType string
		rootPath    string
	}{
		{
			name:        "LocalFsStorage",
			storageType: "local",
			rootPath:    "/tmp/TestWriteReadPerf",
		},
		{
			name:        "ObjectStorage",
			storageType: "", // Use default storage type minio-compatible
			rootPath:    "", // No need to specify path when using default storage
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// ### Create client
			cfg, err := config.NewConfiguration("../../config/woodpecker.yaml")
			assert.NoError(t, err)

			if tc.storageType != "" {
				cfg.Woodpecker.Storage.Type = tc.storageType
			}
			if tc.rootPath != "" {
				cfg.Woodpecker.Storage.RootPath = tc.rootPath
			}

			client, err := woodpecker.NewEmbedClientFromConfig(context.Background(), cfg)
			if err != nil {
				fmt.Println(err)
			}

			// ###  CreateLog if not exists
			client.CreateLog(context.Background(), "test_log")

			// ### OpenLog
			logHandle, openErr := client.OpenLog(context.Background(), "test_log")
			if openErr != nil {
				fmt.Printf("Open log failed, err:%v\n", openErr)
				panic(openErr)
			}
			logHandle.GetName()

			//	### OpenWriter
			logWriter, openWriterErr := logHandle.OpenLogWriter(context.Background())
			if openWriterErr != nil {
				fmt.Printf("Open writer failed, err:%v\n", openWriterErr)
				panic(openWriterErr)
			}

			// gen static data
			payloadStaticData, err := generateRandomBytes(entrySize) // 1KB per row
			assert.NoError(t, err)

			// ### Write
			successCount := 0
			writingResultChan := make([]<-chan *log.WriteResult, 0) // 10M*1k=10GB
			writingMessages := make([]*log.WriterMessage, 0)
			failMessages := make([]*log.WriterMessage, 0)
			for i := 0; i < writeCount; i++ {
				// append async
				msg := &log.WriterMessage{
					Payload: payloadStaticData,
					Properties: map[string]string{
						"key": fmt.Sprintf("value%d", i),
					},
				}
				resultChan := logWriter.WriteAsync(context.Background(), msg)
				writingResultChan = append(writingResultChan, resultChan)
				writingMessages = append(writingMessages, msg)

				// wait for batch finish
				if len(writingMessages)%batchCount == 0 { // wait 64000 entries or 64MB to completed
					fmt.Printf("start wait for %d entries\n", len(writingMessages))
					for idx, ch := range writingResultChan {
						writeResult := <-ch
						if writeResult.Err != nil {
							fmt.Printf(writeResult.Err.Error())
							failMessages = append(failMessages, writingMessages[idx])
						} else {
							//fmt.Printf("write success, returned recordId:%v \n", writeResult.LogMessageId)
							successCount++
						}
					}
					fmt.Printf("finish wait for %d entries. success:%d , failed: %d, i: %d \n", len(writingMessages), successCount, len(failMessages), i)
					time.Sleep(1 * time.Second) // wait a moment to avoid too much retry
					writingResultChan = make([]<-chan *log.WriteResult, 0)
					writingMessages = make([]*log.WriterMessage, 0)
					for _, m := range failMessages {
						retryCh := logWriter.WriteAsync(context.Background(), m)
						writingResultChan = append(writingResultChan, retryCh)
						writingMessages = append(writingMessages, m)
					}
					failMessages = make([]*log.WriterMessage, 0)
				}
			}

			// wait&retry the rest
			{
				for idx, ch := range writingResultChan {
					writeResult := <-ch
					if writeResult.Err != nil {
						fmt.Printf(writeResult.Err.Error())
						failMessages = append(failMessages, writingMessages[idx])
					} else {
						successCount++
					}
				}
				time.Sleep(1 * time.Second) // wait a moment to avoid too much retry
				for {
					if len(failMessages) == 0 {
						break
					}
					writingResultChan = make([]<-chan *log.WriteResult, 0)
					writingMessages = make([]*log.WriterMessage, 0)
					for _, m := range failMessages {
						retryCh := logWriter.WriteAsync(context.Background(), m)
						writingResultChan = append(writingResultChan, retryCh)
						writingMessages = append(writingMessages, m)
					}
					failMessages = make([]*log.WriterMessage, 0)
					for idx, ch := range writingResultChan {
						writeResult := <-ch
						if writeResult.Err != nil {
							fmt.Printf(writeResult.Err.Error())
							failMessages = append(failMessages, writingMessages[idx])
						} else {
							successCount++
						}
					}
				}
			}

			// ### close and print result
			fmt.Printf("start close log writer \n")
			closeErr := logWriter.Close(context.Background())
			if closeErr != nil {
				fmt.Printf("close failed, err:%v\n", closeErr)
				panic(closeErr)
			}
			fmt.Printf("Test Write finished  %d entries\n", successCount)
		})
	}
}

func TestReadThroughput(t *testing.T) {
	startGopsAgent()
	startMetrics()
	startReporting()

	testCases := []struct {
		name        string
		storageType string
		rootPath    string
	}{
		{
			name:        "LocalFsStorage",
			storageType: "local",
			rootPath:    "/tmp/TestWriteReadPerf",
		},
		{
			name:        "ObjectStorage",
			storageType: "", // Use default storage type minio-compatible
			rootPath:    "", // No need to specify path when using default storage
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.NewConfiguration("../../config/woodpecker.yaml")
			assert.NoError(t, err)

			if tc.storageType != "" {
				cfg.Woodpecker.Storage.Type = tc.storageType
			}
			if tc.rootPath != "" {
				cfg.Woodpecker.Storage.RootPath = tc.rootPath
			}

			client, err := woodpecker.NewEmbedClientFromConfig(context.Background(), cfg)
			if err != nil {
				fmt.Println(err)
			}

			// ###  CreateLog if not exists
			client.CreateLog(context.Background(), "test_log")

			// ### OpenLog
			logHandle, openErr := client.OpenLog(context.Background(), "test_log")
			if openErr != nil {
				fmt.Printf("Open log failed, err:%v\n", openErr)
				panic(openErr)
			}

			//	### OpenReader
			earliest := log.EarliestLogMessageID()
			logReader, openReaderErr := logHandle.OpenLogReader(context.Background(), &earliest, "TestReadThroughput")
			if openReaderErr != nil {
				fmt.Printf("Open reader failed, err:%v\n", openReaderErr)
				panic(openReaderErr)
			}

			// read loop
			totalEntries := 0
			totalBytes := 0
			for {
				start := time.Now()
				msg, err := logReader.ReadNext(context.Background())
				if err != nil {
					fmt.Printf("read failed, err:%v\n", err)
					break
				} else {
					//fmt.Printf("read success, msg:%v\n", msg)
					cost := time.Now().Sub(start)
					MinioIOBytes.WithLabelValues("0").Observe(float64(len(msg.Payload)))
					MinioIOLatency.WithLabelValues("0").Observe(float64(cost.Milliseconds()))
				}
				totalBytes += len(msg.Payload)
				totalEntries += 1
				if totalEntries%100 == 0 {
					fmt.Printf(" read %d entries, %d bytes success, current msg(seg:%d,entry:%d) \n", totalEntries, totalBytes, msg.Id.SegmentId, msg.Id.EntryId)
				}
			}
			fmt.Printf("final read %d success \n", totalEntries)
			fmt.Printf("Test Read finished\n")
		})
	}
}

func TestReadFromEarliest(t *testing.T) {
	startGopsAgent()

	testCases := []struct {
		name        string
		storageType string
		rootPath    string
	}{
		{
			name:        "LocalFsStorage",
			storageType: "local",
			rootPath:    "/tmp/TestWriteReadPerf",
		},
		{
			name:        "ObjectStorage",
			storageType: "", // Use default storage type minio-compatible
			rootPath:    "", // No need to specify path when using default storage
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.NewConfiguration("../../config/woodpecker.yaml")
			assert.NoError(t, err)

			if tc.storageType != "" {
				cfg.Woodpecker.Storage.Type = tc.storageType
			}
			if tc.rootPath != "" {
				cfg.Woodpecker.Storage.RootPath = tc.rootPath
			}

			client, err := woodpecker.NewEmbedClientFromConfig(context.Background(), cfg)
			if err != nil {
				fmt.Println(err)
			}

			// ###  CreateLog if not exists
			client.CreateLog(context.Background(), "test_log")

			// ### OpenLog
			logHandle, openErr := client.OpenLog(context.Background(), "test_log")
			if openErr != nil {
				fmt.Printf("Open log failed, err:%v\n", openErr)
				panic(openErr)
			}

			//	### OpenReader
			start := log.EarliestLogMessageID()
			logReader, openReaderErr := logHandle.OpenLogReader(context.Background(), &start, "TestReadFromEarliest")
			if openReaderErr != nil {
				fmt.Printf("Open reader failed, err:%v\n", openReaderErr)
				panic(openReaderErr)
			}

			// read loop
			for {
				msg, err := logReader.ReadNext(context.Background())
				if err != nil {
					fmt.Printf("read failed, err:%v\n", err)
					t.Error(err)
					break
				} else {
					fmt.Printf("read success, msg:%v\n", msg)
				}
			}
			fmt.Println("Test Read finished")
		})
	}
}

func TestReadFromLatest(t *testing.T) {
	startGopsAgent()

	testCases := []struct {
		name        string
		storageType string
		rootPath    string
	}{
		{
			name:        "LocalFsStorage",
			storageType: "local",
			rootPath:    "/tmp/TestWriteReadPerf",
		},
		{
			name:        "ObjectStorage",
			storageType: "", // Use default storage type minio-compatible
			rootPath:    "", // No need to specify path when using default storage
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.NewConfiguration("../../config/woodpecker.yaml")
			assert.NoError(t, err)

			if tc.storageType != "" {
				cfg.Woodpecker.Storage.Type = tc.storageType
			}
			if tc.rootPath != "" {
				cfg.Woodpecker.Storage.RootPath = tc.rootPath
			}

			client, err := woodpecker.NewEmbedClientFromConfig(context.Background(), cfg)
			if err != nil {
				fmt.Println(err)
			}

			// ### OpenLog
			logHandle, openErr := client.OpenLog(context.Background(), "test_log")
			if openErr != nil {
				fmt.Printf("Open log failed, err:%v\n", openErr)
				panic(openErr)
			}

			//	### OpenReader
			latest := log.LatestLogMessageID()
			logReader, openReaderErr := logHandle.OpenLogReader(context.Background(), &latest, "TestReadFromLatest")
			if openReaderErr != nil {
				fmt.Printf("Open reader failed, err:%v\n", openReaderErr)
				panic(openReaderErr)
			}

			more := false
			go func() {
				msg, err := logReader.ReadNext(context.Background())
				if err != nil {
					fmt.Printf("read failed, err:%v\n", err)
					t.Error(err)
				} else {
					more = true
					fmt.Printf("read success, msg:%v\n", msg)
				}
			}()
			time.Sleep(time.Second * 2)
			assert.False(t, more, "should read nothing and timeout")
		})
	}
}

func TestReadFromSpecifiedPosition(t *testing.T) {
	startGopsAgent()

	testCases := []struct {
		name        string
		storageType string
		rootPath    string
	}{
		{
			name:        "LocalFsStorage",
			storageType: "local",
			rootPath:    "/tmp/TestWriteReadPerf",
		},
		{
			name:        "ObjectStorage",
			storageType: "", // Use default storage type minio-compatible
			rootPath:    "", // No need to specify path when using default storage
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.NewConfiguration("../../config/woodpecker.yaml")
			assert.NoError(t, err)

			if tc.storageType != "" {
				cfg.Woodpecker.Storage.Type = tc.storageType
			}
			if tc.rootPath != "" {
				cfg.Woodpecker.Storage.RootPath = tc.rootPath
			}

			client, err := woodpecker.NewEmbedClientFromConfig(context.Background(), cfg)
			if err != nil {
				fmt.Println(err)
			}

			// ### OpenLog
			logHandle, openErr := client.OpenLog(context.Background(), "test_log")
			if openErr != nil {
				fmt.Printf("Open log failed, err:%v\n", openErr)
				panic(openErr)
			}

			//	### OpenReader
			start := &log.LogMessageId{
				SegmentId: 5,
				EntryId:   0,
			}
			logReader, openReaderErr := logHandle.OpenLogReader(context.Background(), start, "TestReadFromSpecifiedPosition")
			if openReaderErr != nil {
				fmt.Printf("Open reader failed, err:%v\n", openReaderErr)
				panic(openReaderErr)
			}

			// read loop
			for {
				msg, err := logReader.ReadNext(context.Background())
				if err != nil {
					fmt.Printf("read failed, err:%v\n", err)
					t.Error(err)
					break
				} else {
					fmt.Printf("read success, msg:%v\n", msg)
				}
			}

			fmt.Println("Test Read finished")
		})
	}
}
