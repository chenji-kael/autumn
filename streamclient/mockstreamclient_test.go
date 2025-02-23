package streamclient

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/journeymidnight/autumn/proto/pb"
	"github.com/journeymidnight/autumn/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestBlock(size uint32) *pb.Block {
	data := make([]byte, size)
	utils.SetRandStringBytes(data)
	rand.Seed(time.Now().UnixNano())
	return &pb.Block{
		CheckSum:    utils.AdlerCheckSum(data),
		BlockLength: size,
		Data:        data,
	}
}

func TestAppendReadBlocks(t *testing.T) {
	b := newTestBlock(512)
	client := NewMockStreamClient("log")
	bReader := client.(BlockReader)
	defer client.Close()
	exID, offsets, err := client.Append(context.Background(), []*pb.Block{b})
	assert.Nil(t, err)

	bs, err := bReader.Read(context.Background(), exID, offsets[0], 1)
	assert.Nil(t, err)
	assert.Equal(t, b.Data, bs[0].Data)
}

func TestAppendReadEntries(t *testing.T) {
	cases := []*pb.EntryInfo{
		{
			Log: &pb.Entry{
				Key:   []byte("a"),
				Value: []byte("xx"),
			},
		},
		{
			Log: &pb.Entry{
				Key:   []byte("b"),
				Value: []byte("xx"),
			},
		},
	}

	client := NewMockStreamClient("log")
	defer client.Close()
	eID, tail, err := client.AppendEntries(context.Background(), cases)

	require.NoError(t, err)
	require.Equal(t, uint32(512+512+4096), tail)

	//GC read
	iter := client.NewLogEntryIter(ReadOption{}.WithReadFromStart())

	//小value在GC时,一个block只返回自己的大小, 上面的entry全部可以GC
	for {
		ok, err := iter.HasNext()
		require.NoError(t, err)
		if !ok {
			break
		}
		ei := iter.Next()
		require.Equal(t, []byte(nil), ei.Log.Key)
	}

	iter = client.NewLogEntryIter(ReadOption{}.WithReadFromStart().WithReplay())

	expectedKeys := [][]byte{
		[]byte("a"),
		[]byte("b"),
	}

	var ans [][]byte
	for {
		ok, err := iter.HasNext()
		require.NoError(t, err)
		if !ok {
			break
		}
		ei := iter.Next()
		ans = append(ans, ei.Log.Key)
	}
	require.Equal(t, expectedKeys, ans)

	ans = ans[:0]
	_, _, err = client.AppendEntries(context.Background(), cases)
	require.NoError(t, err)

	iter = client.NewLogEntryIter(ReadOption{}.WithReadFromStart().WithReadFrom(eID, tail).WithReplay())
	for {
		ok, err := iter.HasNext()
		require.NoError(t, err)
		if !ok {
			break
		}
		ei := iter.Next()
		ans = append(ans, ei.Log.Key)
	}
	require.Equal(t, expectedKeys, ans)
}

func TestAppendReadBigBlocks(t *testing.T) {
	cases := []*pb.EntryInfo{
		{
			Log: &pb.Entry{
				Key:   []byte("a"),
				Value: []byte("xx"),
			},
		},
		{
			Log: &pb.Entry{
				Key:   []byte("b"),
				Value: []byte(fmt.Sprintf("%01048576d", 10)),
			},
		},
	}
	client := NewMockStreamClient("log")
	defer client.Close()
	_, _, err := client.AppendEntries(context.Background(), cases)

	require.NoError(t, err)

	iter := client.NewLogEntryIter(ReadOption{}.WithReadFromStart().WithReplay())
	var ans [][]byte
	for {
		ok, err := iter.HasNext()
		require.NoError(t, err)
		if !ok {
			break
		}
		ei := iter.Next()
		ans = append(ans, ei.Log.Key)
	}
	require.Equal(t, [][]byte{[]byte("a"), []byte("b")}, ans)

}

func TestSplitExtent(t *testing.T) {
	cases := []*pb.EntryInfo{
		{
			Log: &pb.Entry{
				Key:   []byte("a"),
				Value: []byte("xx"),
			},
		},
		{
			Log: &pb.Entry{
				Key:   []byte("b"),
				Value: []byte(fmt.Sprintf("%01048576d", 10)), //1MB
			},
		},
		{
			Log: &pb.Entry{
				Key:   []byte("c"),
				Value: []byte("xx"),
			},
		},
	}

	client := NewMockStreamClient("log").(*MockStreamClient)
	defer client.Close()

	_, _, err := client.AppendEntries(context.Background(), cases)
	_, _, err = client.AppendEntries(context.Background(), cases)
	require.NoError(t, err)

	l := len(client.exs)

	p := client.exs[1].ID
	frontStream, _, err := client.Truncate(context.Background(), p)
	require.NoError(t, err)

	truncStream := OpenMockStreamClient(frontStream)
	defer truncStream.Close()

	//fmt.Printf("len[%d] split to %d vs %d\n", l, len(newStream.ExtentIDs), len(client.exs))
	require.Equal(t, l, len(frontStream.ExtentIDs)+len(client.exs))

	iter := client.NewLogEntryIter(ReadOption{}.WithReadFromStart().WithReplay())
	for {
		ok, err := iter.HasNext()
		require.NoError(t, err)
		if !ok {
			break
		}
		iter.Next()
		//ei := iter.Next()
		//fmt.Printf("%s\n", ei.Log.Key)
	}

}
