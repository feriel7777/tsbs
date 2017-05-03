// bulk_load_cassandra loads a Cassandra daemon with data from stdin.
//
// The caller is responsible for assuring that the database is empty before
// bulk load.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
)

// Program option vars:
var (
	postgresConnect  string
	workers          int
	batchSize        int
	doLoad           bool
	makeHypertable   bool
	logBatches       bool
	tagIndex         string
	fieldIndex       string
	fieldIndexCount  int
	reportingPeriod  int
	numberPartitions int
	columnCount      int64
	rowCount         int64
)

type hypertableBatch struct {
	hypertable string
	rows       []string
}

// Global vars
var (
	batchChan    chan *hypertableBatch
	inputDone    chan struct{}
	workersGroup sync.WaitGroup
)

// Parse args:
func init() {
	flag.StringVar(&postgresConnect, "postgres", "host=postgres user=postgres sslmode=disable", "Postgres connection url")

	flag.IntVar(&batchSize, "batch-size", 10000, "Batch size (input items).")
	flag.IntVar(&workers, "workers", 1, "Number of parallel requests to make.")

	flag.BoolVar(&doLoad, "do-load", true, "Whether to write data. Set this flag to false to check input read speed.")
	flag.BoolVar(&makeHypertable, "make-hypertable", true, "Whether to make the table a hypertable. Set this flag to false to check input write speed and how much the insert logic slows things down.")
	flag.BoolVar(&logBatches, "log-batches", false, "Whether to time individual batches.")

	flag.StringVar(&tagIndex, "tag-index", "VALUE-TIME,TIME-VALUE", "index types for tags (comma deliminated)")
	flag.StringVar(&fieldIndex, "field-index", "TIME-VALUE", "index types for tags (comma deliminated)")
	flag.IntVar(&fieldIndexCount, "field-index-count", -1, "Number of indexed fields (-1 for all)")
	flag.IntVar(&numberPartitions, "number_partitions", 1, "Number of patitions")
	flag.IntVar(&reportingPeriod, "reporting-period", 1000, "Period to report stats")

	flag.Parse()
}

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	if doLoad {
		initBenchmarkDB(postgresConnect, scanner)
	} else {
		//read the header
		for scanner.Scan() {
			if len(scanner.Bytes()) == 0 {
				break
			}
		}
	}

	batchChan = make(chan *hypertableBatch, workers)
	inputDone = make(chan struct{})

	for i := 0; i < workers; i++ {
		workersGroup.Add(1)
		go processBatches(postgresConnect)
	}

	go report(reportingPeriod)

	start := time.Now()
	rowsRead := scan(batchSize, scanner)

	<-inputDone
	close(batchChan)
	workersGroup.Wait()
	end := time.Now()
	took := end.Sub(start)
	columnsRead := columnCount
	rowRate := float64(rowsRead) / float64(took.Seconds())
	columnRate := float64(columnsRead) / float64(took.Seconds())

	fmt.Printf("loaded %d rows in %fsec with %d workers (mean rate %f/sec)\n", rowsRead, took.Seconds(), workers, rowRate)
	fmt.Printf("loaded %d columns in %fsec with %d workers (mean rate %f/sec)\n", columnsRead, took.Seconds(), workers, columnRate)
}

func report(periodMs int) {
	c := time.Tick(time.Duration(periodMs) * time.Millisecond)
	start := time.Now()
	prevTime := start
	prevColCount := int64(0)
	prevRowCount := int64(0)

	for now := range c {
		colCount := atomic.LoadInt64(&columnCount)
		rowCount := atomic.LoadInt64(&rowCount)

		took := now.Sub(prevTime)
		colrate := float64(colCount-prevColCount) / float64(took.Seconds())
		rowrate := float64(rowCount-prevRowCount) / float64(took.Seconds())
		overallRowrate := float64(rowCount) / float64(now.Sub(start).Seconds())

		fmt.Printf("REPORT: time %d col rate %f/sec row rate %f/sec (period) %f/sec (total) total rows %E\n", now.Unix(), colrate, rowrate, overallRowrate, float64(rowCount))

		prevColCount = colCount
		prevRowCount = rowCount
		prevTime = now
	}

}

// scan reads lines from stdin. It expects input in the TimescaleDB format.
func scan(itemsPerBatch int, scanner *bufio.Scanner) int64 {
	batch := make(map[string][]string) // hypertable => copy lines
	var n int
	var linesRead int64
	for scanner.Scan() {
		linesRead++

		parts := strings.SplitN(scanner.Text(), ",", 2) //hypertable, copy line
		hypertable := parts[0]

		batch[hypertable] = append(batch[hypertable], parts[1])

		n++
		if n >= itemsPerBatch {
			for hypertable, rows := range batch {
				batchChan <- &hypertableBatch{hypertable, rows}
			}

			batch = make(map[string][]string)
			n = 0
		}
	}

	if err := scanner.Err(); err != nil {
		log.Fatalf("Error reading input: %s", err.Error())
	}

	// Finished reading input, make sure last batch goes out.
	if n > 0 {
		for hypertable, rows := range batch {
			batchChan <- &hypertableBatch{hypertable, rows}
		}
	}

	// Closing inputDone signals to the application that we've read everything and can now shut down.
	close(inputDone)

	itemsRead := linesRead

	return itemsRead
}

// processBatches reads byte buffers from batchChan and writes them to the target server, while tracking stats on the write.
func processBatches(postgresConnect string) {
	dbBench := sqlx.MustConnect("postgres", postgresConnect+" dbname=benchmark")
	defer dbBench.Close()

	columnCountWorker := int64(0)
	for hypertableBatch := range batchChan {
		if !doLoad {
			continue
		}

		hypertable := hypertableBatch.hypertable
		start := time.Now()

		tx := dbBench.MustBegin()
		copyCmd := fmt.Sprintf("COPY \"%s\" FROM STDIN", hypertable)

		stmt, err := tx.Prepare(copyCmd)
		if err != nil {
			panic(err)
		}
		for _, line := range hypertableBatch.rows {
			sp := strings.Split(line, ",")
			in := make([]interface{}, len(sp))
			columnCountWorker += int64(len(sp))
			for ind, value := range sp {
				if ind == 0 {
					timeInt, err := strconv.ParseInt(value, 10, 64)
					if err != nil {
						panic(err)
					}
					secs := timeInt / 1000000000
					in[ind] = time.Unix(secs, timeInt%1000000000).Format("2006-01-02 15:04:05.999999 -7:00")
				} else {
					in[ind] = value
				}
			}
			_, err = stmt.Exec(in...)
			if err != nil {
				panic(err)
			}
		}
		atomic.AddInt64(&columnCount, columnCountWorker)
		atomic.AddInt64(&rowCount, int64(len(hypertableBatch.rows)))
		columnCountWorker = 0

		err = stmt.Close()
		if err != nil {
			panic(err)
		}

		err = tx.Commit()
		if err != nil {
			panic(err)
		}

		if logBatches {
			now := time.Now()
			took := now.Sub(start)
			fmt.Printf("BATCH: time %d batchsize %d row rate %f/sec\n", now.Unix(), batchSize, float64(batchSize)/float64(took.Seconds()))
		}

	}
	workersGroup.Done()
}

func initBenchmarkDB(postgresConnect string, scanner *bufio.Scanner) {
	db := sqlx.MustConnect("postgres", postgresConnect)
	defer db.Close()
	db.MustExec("DROP DATABASE IF EXISTS benchmark")
	db.MustExec("CREATE DATABASE benchmark")

	dbBench := sqlx.MustConnect("postgres", postgresConnect+" dbname=benchmark")
	defer dbBench.Close()

	if makeHypertable {
		dbBench.MustExec("CREATE EXTENSION IF NOT EXISTS timescaledb CASCADE")
		dbBench.MustExec("SELECT setup_timescaledb()")
	}

	for scanner.Scan() {
		if len(scanner.Bytes()) == 0 {
			return
		}

		parts := strings.Split(scanner.Text(), ",")

		hypertable := parts[0]
		partitioningField := ""
		fieldDef := []string{}
		indexes := []string{}

		for idx, field := range parts[1:] {
			if len(field) == 0 {
				continue
			}
			fieldType := "DOUBLE PRECISION"
			idxType := fieldIndex
			if idx == 0 {
				partitioningField = field
				fieldType = "TEXT"
				idxType = tagIndex
			}

			fieldDef = append(fieldDef, fmt.Sprintf("%s %s", field, fieldType))
			if fieldIndexCount == -1 || idx <= fieldIndexCount {
				for _, idx := range strings.Split(idxType, ",") {
					indexDef := ""
					if idx == "TIME-VALUE" {
						indexDef = fmt.Sprintf("(time, %s)", field)
					} else if idx == "VALUE-TIME" {
						indexDef = fmt.Sprintf("(%s,time)", field)
					} else if idx != "" {
						panic(fmt.Sprintf("Unknown index type %v", idx))
					}

					if idx != "" {
						indexes = append(indexes, fmt.Sprintf("CREATE INDEX ON %s %s", hypertable, indexDef))
					}
				}
			}
		}
		dbBench.MustExec(fmt.Sprintf("CREATE TABLE %s (time timestamptz, %s)", hypertable, strings.Join(fieldDef, ",")))

		for _, idxDef := range indexes {
			dbBench.MustExec(idxDef)
		}

		if makeHypertable {
			dbBench.MustExec(
				fmt.Sprintf("SELECT create_hypertable('%s'::regclass, 'time'::name, partitioning_column => '%s'::name, number_partitions => %v::smallint, chunk_time_interval => 28800000000)",
					hypertable, partitioningField, numberPartitions))
		}
	}
}
