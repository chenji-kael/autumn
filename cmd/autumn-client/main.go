package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/journeymidnight/autumn/manager/pmclient"
	"github.com/journeymidnight/autumn/manager/smclient"
	"github.com/journeymidnight/autumn/utils"
	"github.com/journeymidnight/autumn/xlog"
	_ "github.com/journeymidnight/autumn/xlog"
	"github.com/pkg/errors"
	"github.com/urfave/cli/v2"
	"go.uber.org/zap/zapcore"
	_ "go.uber.org/zap/zapcore"
)

type Result struct {
	Key       string
	StartTime float64 //time.Now().Second
	Elapsed   float64
}

type BenchType string

const (
	READ_T  BenchType = "read"
	WRITE_T           = "write"
)

func benchmark(pmAddrs []string, op BenchType, threadNum int, duration int, size int) error {

	pm := pmclient.NewAutumnPMClient(pmAddrs)
	if err := pm.Connect(); err != nil {
		return err
	}
	client := NewAutumnLib(pmAddrs)
	//defer client.Close()

	if err := client.Connect(); err != nil {
		return err
	}

	stopper := utils.NewStopper()

	//prepare data
	data := make([]byte, size)
	utils.SetRandStringBytes(data)

	var lock sync.Mutex //protect results
	var results []Result
	benchStartTime := time.Now()
	var count uint64
	var totalSize uint64

	done := make(chan struct{})

	start := time.Now()
	livePrint := func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		fmt.Print("\033[s") // save the cursor position

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				//https://stackoverflow.com/questions/56103775/how-to-print-formatted-string-to-the-same-line-in-stdout-with-go
				//how to print in one line
				fmt.Print("\033[u\033[K")
				ops := atomic.LoadUint64(&count) / uint64(time.Now().Sub(start).Seconds())
				throughput := float64(atomic.LoadUint64(&totalSize)) / time.Now().Sub(start).Seconds()
				fmt.Printf("ops:%d/s  throughput:%s", ops, utils.HumanReadableThroughput(throughput))
			}
		}
	}

	go func() {
		for i := 0; i < threadNum; i++ {
			loop := 0 //sample to record lantency
			t := i
			stopper.RunWorker(func() {
				j := 0
				var ctx context.Context
				var cancel context.CancelFunc
				for {

					select {
					case <-stopper.ShouldStop():
						if cancel != nil {
							cancel()
						}
						return
					default:
						write := func(t int) {
							key := fmt.Sprintf("test%d_%d", t, j)
							ctx, cancel = context.WithCancel(context.Background())
							start := time.Now()
							err := client.Put(ctx, []byte(key), data)
							cancel()
							end := time.Now()
							j++
							if err != nil {
								fmt.Printf("%v\n", err)
								return
							}
							if loop%3 == 0 {
								lock.Lock()
								results = append(results, Result{
									Key:       key,
									StartTime: start.Sub(benchStartTime).Seconds(),
									Elapsed:   end.Sub(start).Seconds(),
								})
								lock.Unlock()
							}
							atomic.AddUint64(&totalSize, uint64(size))
							atomic.AddUint64(&count, 1)
							loop++
						}
						read := func(t int) {}
						switch op {
						case "read":
							read(t)
						case "write":
							write(t)
						default:
							fmt.Println("bench type is wrong")
							return
						}

					}

				}
			})

		}
		stopper.Wait()
		close(done)
	}()

	go livePrint()

	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM,
		syscall.SIGQUIT, syscall.SIGHUP, syscall.SIGUSR1)
	select {
	case <-signalCh:
		stopper.Stop()
	case <-time.After(time.Duration(duration) * time.Second):
		stopper.Stop()
	case <-done:
		break
	}
	//write down result

	sort.Slice(results, func(i, j int) bool {
		return results[i].StartTime < results[i].StartTime
	})

	var fileName string
	switch op {
	case READ_T:
		fileName = "rresult.json"
	case WRITE_T:
		fileName = "result.json"
	default:
		return errors.Errorf("benchtype error")
	}

	f, err := os.OpenFile(fileName, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0644)
	defer f.Close()
	if err == nil {
		out, err := json.Marshal(results)
		if err == nil {
			f.Write(out)
		} else {
			fmt.Println("failed to write result.json")
		}
	}
	printSummary(time.Now().Sub(start), atomic.LoadUint64(&count), atomic.LoadUint64(&totalSize), threadNum, size)

	return nil
}

func bootstrap(c *cli.Context) error {
	smAddrs := utils.SplitAndTrim(c.String("smAddr"), ",")
	pmAddrs := utils.SplitAndTrim(c.String("pmAddr"), ",")

	smc := smclient.NewSMClient(smAddrs)
	if err := smc.Connect(); err != nil {
		return err
	}

	pmc := pmclient.NewAutumnPMClient(pmAddrs)
	if err := pmc.Connect(); err != nil {
		return err
	}
	fmt.Println("Bootstrap")
	pss := pmc.GetPSInfo()

	if len(pss) == 0 {
		return errors.New("no Partition Servers...")
	}
	//choose the first one

	log, _, err := smc.CreateStream(context.Background())
	if err != nil {
		return err
	}
	row, _, err := smc.CreateStream(context.Background())
	if err != nil {
		return err
	}

	partID, err := pmc.Bootstrap(log.StreamID, row.StreamID, pss[0].PSID)
	if err != nil {
		return err
	}
	fmt.Printf("bootstrap succeed, created new range partition %d on %d\n", partID, pss[0].PSID)
	return nil
}

func del(c *cli.Context) error {
	pmAddr := utils.SplitAndTrim(c.String("pmAddr"), ",")
	client := NewAutumnLib(pmAddr)
	//defer client.Close()
	if err := client.Connect(); err != nil {
		return err
	}
	key := c.Args().First()
	if len(key) == 0 {
		return errors.New("no key")
	}

	return client.Delete(context.Background(), []byte(key))

}

func get(c *cli.Context) error {
	pmAddr := utils.SplitAndTrim(c.String("pmAddr"), ",")
	client := NewAutumnLib(pmAddr)
	//defer client.Close()

	if err := client.Connect(); err != nil {
		return err
	}
	key := c.Args().First()
	if len(key) == 0 {
		return errors.New("no key")
	}

	value, err := client.Get(context.Background(), []byte(key))
	if err != nil {
		return errors.Errorf(("get key:%s failed: reason:%s"), key, err)
	}
	//print the raw data to stdout, fmt.Println does not work
	binary.Write(os.Stdout, binary.LittleEndian, value)

	return nil
}

func autumnRange(c *cli.Context) error {
	pmAddr := utils.SplitAndTrim(c.String("pmAddr"), ",")
	if len(pmAddr) == 0 {
		return errors.Errorf("pmAddr is nil")
	}
	client := NewAutumnLib(pmAddr)
	//defer client.Close()

	if err := client.Connect(); err != nil {
		return err
	}
	prefix := c.Args().First()
	/*
		if len(prefix) == 0 {
			return errors.New("no key")
		}
	*/
	out, err := client.Range(context.Background(), []byte(prefix), []byte(prefix))
	if err != nil {
		return err
	}
	for i := range out {
		fmt.Printf("%s\n", out[i])
	}
	return nil
}

//FIXME: grpc stream is better to send big values
func put(c *cli.Context) error {
	pmAddr := utils.SplitAndTrim(c.String("pmAddr"), ",")
	if len(pmAddr) == 0 {
		return errors.Errorf("pmAddr is nil")
	}
	client := NewAutumnLib(pmAddr)
	//defer client.Close()

	if err := client.Connect(); err != nil {
		return err
	}
	key := c.Args().First()
	if len(key) == 0 {
		return errors.New("no key")
	}
	fileName := c.Args().Get(1)
	if len(fileName) == 0 {
		return errors.New("no fileName")
	}
	value, err := ioutil.ReadFile(fileName)
	if err != nil {
		return errors.Errorf("read file %s: err: %s", fileName, err.Error())
	}
	if err := client.Put(context.Background(), []byte(key), value); err != nil {
		return errors.Errorf(("put key:%s failed: reason:%s"), key, err)
	}
	fmt.Println("success")
	return nil
}

func info(c *cli.Context) error {
	smAddrs := utils.SplitAndTrim(c.String("smAddr"), ",")
	client := smclient.NewSMClient(smAddrs)
	if err := client.Connect(); err != nil {
		return err
	}
	streams, extents, err := client.StreamInfo(context.Background(), nil)
	if err != nil {
		return err
	}

	nodes, err := client.NodesInfo(context.Background())
	if err != nil {
		return err
	}
	fmt.Printf("%v\n", streams)
	fmt.Printf("%v\n", extents)
	fmt.Printf("%v\n", nodes)
	return nil
}

/*
func alloc(c *cli.Context) error {
	cluster := c.String("cluster")
	client := smclient.NewSMClient([]string{cluster})
	if err := client.Connect(); err != nil {
		return err
	}
	s, e, err := client.CreateStream(context.Background())
	if err != nil {
		return err
	}
	fmt.Printf("%v\n", s)
	fmt.Printf("%v\n", e)
	return nil
}
*/

func main() {
	xlog.InitLog([]string{"client.log"}, zapcore.DebugLevel)
	app := cli.NewApp()
	app.Name = "autumn"
	app.Usage = "autumn subcommand"
	app.Commands = []*cli.Command{
		{
			Name:  "info",
			Usage: "info --smAddrs <path>",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "smAddr", Value: "127.0.0.1:3401"},
			},
			Action: info,
		},

		{
			Name:  "bootstrap",
			Usage: "bootstrap --pmAddr <addrs> --smAddr <addrs>",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "smAddr", Value: "127.0.0.1:3401"},
				&cli.StringFlag{Name: "pmAddr", Value: "127.0.0.1:3401"},
			},
			Action: bootstrap,
		},

		{
			Name:  "put",
			Usage: "put --pmAddr <addrs> <KEY> <FILE>",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "pmAddr", Value: "127.0.0.1:3401"},
			},
			Action: put,
		},
		{
			Name:  "get",
			Usage: "get --pmAddr <addrs> <KEY>",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "pmAddr", Value: "127.0.0.1:3401"},
			},
			Action: get,
		},
		{
			Name:  "del",
			Usage: "del --pmAddr <addrs> <KEY>",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "pmAddr", Value: "127.0.0.1:3401"},
			},
			Action: del,
		},
		{
			Name:  "wbench",
			Usage: "wbench --pmAddr <addrs> --thread <num> --duration <duration>",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "pmAddr", Value: "127.0.0.1:3401"},
				&cli.IntFlag{Name: "thread", Value: 4, Aliases: []string{"t"}},
				&cli.IntFlag{Name: "duration", Value: 10, Aliases: []string{"d"}},
				&cli.IntFlag{Name: "size", Value: 8192, Aliases: []string{"s"}},
			},
			Action: wbench,
		},
		{
			Name:   "plot",
			Usage:  "plot <file.json>",
			Action: plot,
		},
		{
			Name:  "ls",
			Usage: "ls --pmAddr <addrs> <prefix>",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "pmAddr", Value: "127.0.0.1:3401"},
			},
			Action: autumnRange,
		},
	}
	err := app.Run(os.Args)
	if err != nil {
		fmt.Println(err)
	}

}

func wbench(c *cli.Context) error {
	threadNum := c.Int("thread")
	pmAddr := c.String("pmAddr")
	duration := c.Int("duration")
	size := c.Int("size")
	pmAddrs := utils.SplitAndTrim(pmAddr, ",")
	return benchmark(pmAddrs, WRITE_T, threadNum, duration, size)
}

func printSummary(elapsed time.Duration, totalCount uint64, totalSize uint64, threadNum int, size int) {
	if elapsed.Seconds() < 1e-9 {
		return
	}
	fmt.Printf("\nSummary\n")
	fmt.Printf("Threads :%d\n", threadNum)
	fmt.Printf("Size    :%d\n", size)
	fmt.Printf("Time taken for tests :%v seconds\n", elapsed.Seconds())
	fmt.Printf("Complete requests :%d\n", totalCount)
	fmt.Printf("Total transferred :%d bytes\n", totalSize)
	fmt.Printf("Requests per second :%.2f [#/sec]\n", float64(totalCount)/elapsed.Seconds())
	t := float64(totalSize) / elapsed.Seconds()
	fmt.Printf("Thoughput per sencond :%s\n", utils.HumanReadableThroughput(t))
}
