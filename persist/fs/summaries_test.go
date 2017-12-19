// Copyright (c) 2017 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package fs

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/m3db/m3db/digest"
	"github.com/m3db/m3db/persist/encoding"
	"github.com/m3db/m3db/persist/encoding/msgpack"
	"github.com/m3db/m3db/ts"

	"github.com/m3db/m3x/checked"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"github.com/stretchr/testify/assert"
)

func TestIndexLookupWriteRead(t *testing.T) {
	// Define property test function which will be passed various propTestInputs
	propertyFunc := func(input propTestInput) (bool, error) {
		// Filter out duplicate IDs
		writes := []generatedWrite{}
		unique := map[string]struct{}{}
		for _, write := range input.realWrites {
			s := string(write.id.Data().Get())
			if _, ok := unique[s]; ok {
				continue
			}
			unique[s] = struct{}{}
			writes = append(writes, write)
		}

		// Create a temporary directory for each test run
		dir, err := ioutil.TempDir("", "testdb")
		if err != nil {
			return false, err
		}
		filePathPrefix := filepath.Join(dir, "")
		defer os.RemoveAll(dir)

		options := NewOptions().
			// Make sure that every index entry is also in the summaries file for the
			// sake of verifying behavior
			SetIndexSummariesPercent(1).
			SetFilePathPrefix(filePathPrefix).
			SetWriterBufferSize(testWriterBufferSize)
		shard := input.shard

		// Instantiate a writer and write the test data
		w, err := NewWriter(options)
		if err != nil {
			return false, fmt.Errorf("err creating writer: %v, ", err)
		}
		err = w.Open(testNs1ID, testBlockSize, shard, testWriterStart)
		if err != nil {
			return false, fmt.Errorf("err opening writer: %v, ", err)
		}
		shardDirPath := ShardDirPath(filePathPrefix, testNs1ID, shard)
		err = writeTestSummariesData(w, writes)
		if err != nil {
			return false, fmt.Errorf("err writing test summaries data: %v, ", err)
		}

		// Figure out the offsets for the writes so we have something to compare
		// our results against
		expectedIndexFileOffsets, err := readIndexFileOffsets(
			shardDirPath, len(writes), testWriterStart)
		if err != nil {
			return false, fmt.Errorf("err reading index file offsets: %v", err)
		}

		// Read the summaries file into memory
		summariesFilePath := filesetPathFromTime(
			shardDirPath, testWriterStart, summariesFileSuffix)
		summariesFile, err := os.Open(summariesFilePath)
		if err != nil {
			return false, fmt.Errorf("err opening summaries file: %v, ", err)
		}
		summariesFdWithDigest := digest.NewFdWithDigestReader(options.InfoReaderBufferSize())
		expectedSummariesDigest := calculateExpectedChecksum(t, summariesFilePath)
		decoder := msgpack.NewDecoder(options.DecodingOptions())
		indexLookup, err := readIndexLookupFromSummariesFile(
			summariesFile, summariesFdWithDigest, expectedSummariesDigest, decoder, len(writes))
		if err != nil {
			return false, fmt.Errorf("err reading index lookup from summaries file: %v, ", err)
		}

		// Make sure it returns the correct index offset for every ID
		for id, expectedOffset := range expectedIndexFileOffsets {
			foundOffset, ok, err := indexLookup.getNearestIndexFileOffset(ts.StringID(id))
			if err != nil {
				return false, fmt.Errorf("Err locating index file offset for: %s, err: %v", id, err)
			}
			if !ok {
				return false, fmt.Errorf("Unable to locate index file offset for: %s", id)
			}
			if expectedOffset != foundOffset {
				return false, fmt.Errorf(
					"Offsets for: %s do not match, expected: %d, got: %d",
					id, expectedOffset, foundOffset)
			}
		}

		// Filter out any IDs from fake writes that already exist in real writes
		fakeWrites := []generatedWrite{}
		for _, fakeWrite := range input.fakeWrites {
			s := string(fakeWrite.id.Data().Get())
			if _, ok := unique[s]; ok {
				continue
			}
			fakeWrites = append(fakeWrites, fakeWrite)
		}

		// // Make sure it returns false for IDs that do not exist
		// for _, fakeWrite := range fakeWrites {
		// 	_, ok, err := indexLookup.getNearestIndexFileOffset(fakeWrite.id)
		// 	if err != nil {
		// 		return false, fmt.Errorf("Err locating index file offset for: %s, err: %v", fakeWrite.id, err)
		// 	}
		// 	if ok {
		// 		return false, fmt.Errorf("Found locate index file offset for: %s which should not have been found", fakeWrite.id)
		// 	}
		// }

		return true, nil
	}

	parameters := gopter.DefaultTestParameters()
	parameters.Rng.Seed(123456789)
	parameters.MinSuccessfulTests = 100
	props := gopter.NewProperties(parameters)

	props.Property(
		"Index lookup can properly lookup index offsets",
		prop.ForAll(propertyFunc, genPropTestInputs()),
	)

	props.TestingRun(t)
}

func calculateExpectedChecksum(t *testing.T, filePath string) uint32 {
	fileBytes, err := ioutil.ReadFile(filePath)
	assert.NoError(t, err)
	return digest.Checksum(fileBytes)
}

func writeTestSummariesData(w FileSetWriter, writes []generatedWrite) error {
	for _, write := range writes {
		err := w.Write(write.id, write.data, write.checksum)
		if err != nil {
			return err
		}
	}
	return w.Close()
}

type propTestInput struct {
	// IDs to write and assert against
	realWrites []generatedWrite
	// IDs to assert can't be found in the summaries file
	fakeWrites []generatedWrite
	// Shard number to use for the files
	shard uint32
}

type generatedWrite struct {
	id       ts.ID
	data     checked.Bytes
	checksum uint32
}

func genPropTestInputs() gopter.Gen {
	return gopter.CombineGens(
		gen.IntRange(0, 1000),
		gen.IntRange(0, 10),
	).FlatMap(func(input interface{}) gopter.Gen {
		inputs := input.([]interface{})
		numRealWrites := inputs[0].(int)
		numFakeWrites := inputs[1].(int)
		return genPropTestInput(numRealWrites, numFakeWrites)
	}, reflect.TypeOf(propTestInput{}))
}

func genPropTestInput(numRealWrites, numFakeWrites int) gopter.Gen {
	return gopter.CombineGens(
		gen.SliceOfN(numRealWrites, genWrite()),
		gen.SliceOfN(numFakeWrites, genWrite()),
		gen.UInt32(),
	).Map(func(vals []interface{}) propTestInput {
		return propTestInput{
			realWrites: vals[0].([]generatedWrite),
			fakeWrites: vals[1].([]generatedWrite),
			shard:      vals[2].(uint32),
		}
	})
}

func genWrite() gopter.Gen {
	return gopter.CombineGens(
		// gopter will generate random strings, but some of them may be duplicates
		// (which can't normally happen for IDs and breaks this codepath), so we
		// filter down to unique inputs
		gen.AnyString(),
		gen.SliceOfN(100, gen.UInt8()),
	).Map(func(vals []interface{}) generatedWrite {
		id := vals[0].(string)
		data := vals[1].([]byte)

		return generatedWrite{
			id:       ts.StringID(id),
			data:     bytesRefd(data),
			checksum: digest.Checksum(data),
		}
	})
}

func readIndexFileOffsets(shardDirPath string, numEntries int, start time.Time) (map[string]int64, error) {
	indexFilePath := filesetPathFromTime(shardDirPath, start, indexFileSuffix)
	buf, err := ioutil.ReadFile(indexFilePath)
	if err != nil {
		return nil, fmt.Errorf("err reading index file: %v, ", err)
	}

	decoderStream := encoding.NewDecoderStream(buf)
	decoder := msgpack.NewDecoder(NewOptions().DecodingOptions())
	decoder.Reset(decoderStream)

	summariesOffsets := map[string]int64{}
	for read := 0; read < numEntries; read++ {
		offset := int64(len(buf)) - (decoderStream.Remaining())
		entry, err := decoder.DecodeIndexEntry()
		if err != nil {
			return nil, fmt.Errorf("err decoding index entry: %v", err)
		}
		summariesOffsets[string(entry.ID)] = offset
	}
	return summariesOffsets, nil
}
