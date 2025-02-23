package rangepartition

import (
	"fmt"
	"math/rand"
	"sync"
	"testing"

	"github.com/journeymidnight/autumn/manager/pmclient"
	"github.com/journeymidnight/autumn/proto/pb"
	"github.com/journeymidnight/autumn/rangepartition/skiplist"
	"github.com/journeymidnight/autumn/streamclient"
	"github.com/journeymidnight/autumn/utils"
	"github.com/journeymidnight/autumn/xlog"

	"go.uber.org/zap/zapcore"

	"github.com/journeymidnight/autumn/rangepartition/y"
	"github.com/stretchr/testify/require"
)

func init() {
	xlog.InitLog([]string{"rp.log"}, zapcore.DebugLevel)
}

func TestEstimateSize(t *testing.T) {
	bigValue := []byte(fmt.Sprintf("%01048576d", 10)) //1MB
	smallValue := []byte(fmt.Sprintf("%01048d", 10))  //1KB

	entries := []*pb.EntryInfo{
		{Log: &pb.Entry{Key: y.KeyWithTs([]byte("hello"), 0), Value: []byte("test")}},
		{Log: &pb.Entry{Key: y.KeyWithTs([]byte("hello1"), 0), Value: bigValue}},
		{Log: &pb.Entry{Key: y.KeyWithTs([]byte("hello2"), 0), Value: smallValue}},
		{Log: &pb.Entry{Key: y.KeyWithTs([]byte("hello3"), 0), Value: []byte("testasdfasdfasdfasdfasdfafafasdfasdfa"), ExpiresAt: 1243434343434}},
	}

	x := skiplist.NewSkiplist(10 * MB)
	pre := x.MemSize()
	for i := range entries {
		l := int64(estimatedSizeInSkl(entries[i].Log))
		_writeToLSM(x, []*pb.EntryInfo{entries[i]})
		fmt.Printf("%d <= %d\n", x.MemSize()-pre, l)
		require.True(t, x.MemSize()-pre <= l)
		pre = x.MemSize()
	}

}

//helper function for TestEstimateSize.

func _writeToLSM(skl *skiplist.Skiplist, entires []*pb.EntryInfo) int64 {
	for _, entry := range entires {
		if y.ShouldWriteValueToLSM(entry.Log) { // Will include deletion / tombstone case.
			skl.Put(entry.Log.Key,
				y.ValueStruct{
					Value:     entry.Log.Value,
					Meta:      getLowerByte(entry.Log.Meta),
					UserMeta:  getLowerByte(entry.Log.UserMeta),
					ExpiresAt: entry.Log.ExpiresAt,
				})
		} else {
			vp := valuePointer{
				entry.ExtentID,
				entry.Offset,
				uint32(len(entry.Log.Value)),
			}
			skl.Put(entry.Log.Key,
				y.ValueStruct{
					Value:     vp.Encode(),
					Meta:      getLowerByte(entry.Log.Meta) | y.BitValuePointer,
					UserMeta:  getLowerByte(entry.Log.UserMeta),
					ExpiresAt: entry.Log.ExpiresAt,
				})
		}
	}

	return skl.MemSize()
}

func mockOpenStream(si pb.StreamInfo) streamclient.StreamClient {
	return streamclient.OpenMockStreamClient(si)
}

func mockUpdateStream([]pb.StreamInfo) {

}

func runRPTest(t *testing.T, test func(t *testing.T, rp *RangePartition)) {

	logStream := streamclient.NewMockStreamClient("log")
	rowStream := streamclient.NewMockStreamClient("sst")

	defer logStream.Close()
	defer rowStream.Close()
	pmclient := new(pmclient.MockPMClient)
	rp := OpenRangePartition(3, rowStream, logStream, logStream.(streamclient.BlockReader),
		[]byte(""), []byte(""), nil, nil, pmclient, streamclient.OpenMockStreamClient, streamclient.UpdateStreamMock)
	defer func() {
		require.NoError(t, rp.Close())
	}()
	test(t, rp)
}

func TestWriteRead(t *testing.T) {
	runRPTest(t, func(t *testing.T, rp *RangePartition) {
		var wg sync.WaitGroup
		for i := 0; i < 100; i++ {
			wg.Add(1)
			rp.WriteAsync([]byte(fmt.Sprintf("key%d", i)), []byte(fmt.Sprintf("val%d", i)), func(e error) {
				wg.Done()
			})
			//rp.write([]byte(fmt.Sprintf("key%d", i)), []byte(fmt.Sprintf("val%d", i)))
		}
		wg.Wait()

		for i := 0; i < 100; i++ {
			v, err := rp.Get([]byte(fmt.Sprintf("key%d", i)), 300)
			require.NoError(t, err)
			require.Equal(t, []byte(fmt.Sprintf("val%d", i)), v)
		}

	})
}

func TestUpdateRead(t *testing.T) {
	runRPTest(t, func(t *testing.T, rp *RangePartition) {
		var wg sync.WaitGroup
		for i := 0; i < 100; i++ {
			wg.Add(1)
			rp.WriteAsync([]byte("key"), []byte(fmt.Sprintf("val%d", i)), func(e error) {
				wg.Done()
			})
		}
		wg.Wait()

		value, err := rp.Get([]byte("key"), 0)
		require.NoError(t, err)
		require.Equal(t, []byte(fmt.Sprintf("val%d", 99)), value)

	})
}

func TestGetBig(t *testing.T) {
	runRPTest(t, func(t *testing.T, rp *RangePartition) {
		//txnSet(t, db, []byte("key1"), []byte("val1"), 0x08)
		bigValue := []byte(fmt.Sprintf("%01048576d", 10))
		err := rp.Write([]byte("key1"), bigValue)
		require.NoError(t, err)

		v, err := rp.Get([]byte("key1"), 0)

		require.NoError(t, err)
		require.Equal(t, len(bigValue), len(v))

	})

}

func TestReopenRangePartition(t *testing.T) {

	logStream := streamclient.NewMockStreamClient("log")
	rowStream := streamclient.NewMockStreamClient("sst")

	defer logStream.Close()
	defer rowStream.Close()
	pmclient := new(pmclient.MockPMClient)
	rp := OpenRangePartition(3, rowStream, logStream, logStream.(streamclient.BlockReader),
		[]byte(""), []byte(""), nil, nil, pmclient, streamclient.OpenMockStreamClient, streamclient.UpdateStreamMock)

	var wg sync.WaitGroup
	for i := 10; i < 100; i++ {
		wg.Add(1)
		rp.WriteAsync([]byte(fmt.Sprintf("key%d", i)), []byte(fmt.Sprintf("val%d", i)), func(e error) {
			wg.Done()
		})
	}
	wg.Wait()
	rp.Close()

	//reopen with tables
	rp = OpenRangePartition(3, rowStream, logStream, logStream.(streamclient.BlockReader),
		[]byte(""), []byte(""), pmclient.Tables, nil, pmclient, streamclient.OpenMockStreamClient, streamclient.UpdateStreamMock)

	for i := 10; i < 100; i++ {
		v, err := rp.Get([]byte(fmt.Sprintf("key%d", i)), 300)
		if err == errNotFound {
			fmt.Printf("key%d failed\n", i)
			continue
		}
		//require.NoErrorf(t, err, "key%d failed", i)
		require.Equal(t, []byte(fmt.Sprintf("val%d", i)), v)
	}
	rp.Close()
}

func TestReopenRangePartitionWithBig(t *testing.T) {

	logStream := streamclient.NewMockStreamClient("log")
	rowStream := streamclient.NewMockStreamClient("sst")

	defer logStream.Close()
	defer rowStream.Close()
	pmclient := new(pmclient.MockPMClient)
	rp := OpenRangePartition(3, rowStream, logStream, logStream.(streamclient.BlockReader),
		[]byte(""), []byte(""), nil, nil, pmclient, streamclient.OpenMockStreamClient, streamclient.UpdateStreamMock)

	var expectedValue [][]byte
	var wg sync.WaitGroup
	for i := 10; i < 100; i++ {
		wg.Add(1)
		n := 2048 + rand.Int31n(100)
		val := make([]byte, n)
		utils.SetRandStringBytes(val)
		expectedValue = append(expectedValue, val)
		rp.WriteAsync([]byte(fmt.Sprintf("key%d", i)), val, func(e error) {
			wg.Done()
		})
	}
	wg.Wait()
	rp.close(false)

	//reopen with tables
	rp = OpenRangePartition(3, rowStream, logStream, logStream.(streamclient.BlockReader),
		[]byte(""), []byte(""), pmclient.Tables, nil, pmclient, streamclient.OpenMockStreamClient, streamclient.UpdateStreamMock)

	for i := 10; i < 100; i++ {
		v, err := rp.Get([]byte(fmt.Sprintf("key%d", i)), 300)
		if err == errNotFound {
			fmt.Printf("key%d failed\n", i)
			continue
		}
		//require.NoErrorf(t, err, "key%d failed", i)
		require.Equal(t, expectedValue[i-10], v)
	}
	rp.Close()
}

func TestRange(t *testing.T) {
	runRPTest(t, func(t *testing.T, rp *RangePartition) {
		var wg sync.WaitGroup
		for i := 0; i < 100; i++ {
			wg.Add(1)
			rp.WriteAsync([]byte(fmt.Sprintf("key%d", i)), []byte(fmt.Sprintf("val%d", i)), func(e error) {
				wg.Done()
			})
			//rp.write([]byte(fmt.Sprintf("key%d", i)), []byte(fmt.Sprintf("val%d", i)))
		}
		wg.Wait()

		//write twice

		for i := 0; i < 100; i++ {
			wg.Add(1)
			rp.WriteAsync([]byte(fmt.Sprintf("key%d", i)), []byte(fmt.Sprintf("val%d", i)), func(e error) {
				wg.Done()
			})
			//rp.write([]byte(fmt.Sprintf("key%d", i)), []byte(fmt.Sprintf("val%d", i)))
		}
		wg.Wait()

		err := rp.Delete([]byte("key99"))
		require.Nil(t, err)

		//key0, key1 key10,  ... ,k90, key91 ... key98
		var array [][]byte
		array = append(array, []byte("key9"))
		for i := 90; i <= 98; i++ {
			array = append(array, []byte(fmt.Sprintf("key%d", i)))

		}
		out := rp.Range([]byte("key9"), []byte("key9"), 100)

		/* display out
		for _, x := range out {
			binary.Write(os.Stdout, binary.LittleEndian, x)
			fmt.Println()
		}
		*/
		require.Equal(t, array, out)

	})
}
