// Licensed to Apache Software Foundation (ASF) under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Apache Software Foundation (ASF) licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package stream

import (
	"sort"
	"sync"

	"github.com/apache/skywalking-banyandb/api/common"
	"github.com/apache/skywalking-banyandb/pkg/bytes"
	"github.com/apache/skywalking-banyandb/pkg/encoding"
	"github.com/apache/skywalking-banyandb/pkg/fs"
	"github.com/apache/skywalking-banyandb/pkg/logger"
	pbv1 "github.com/apache/skywalking-banyandb/pkg/pb/v1"
	"github.com/apache/skywalking-banyandb/pkg/timestamp"
)

type block struct {
	timestamps  []int64
	elementIDs  []string
	tagFamilies []tagFamily
}

func (b *block) reset() {
	b.timestamps = b.timestamps[:0]
	b.elementIDs = b.elementIDs[:0]

	tff := b.tagFamilies
	for i := range tff {
		tff[i].reset()
	}
	b.tagFamilies = tff[:0]
}

func (b *block) mustInitFromElements(timestamps []int64, elementIDs []string, tagFamilies [][]tagValues) {
	b.reset()
	size := len(timestamps)
	if size == 0 {
		return
	}
	if size != len(tagFamilies) {
		logger.Panicf("the number of timestamps %d must match the number of tagFamilies %d", size, len(tagFamilies))
	}

	assertTimestampsSorted(timestamps)
	b.timestamps = append(b.timestamps, timestamps...)
	b.elementIDs = append(b.elementIDs, elementIDs...)
	b.mustInitFromTags(tagFamilies)
}

func assertTimestampsSorted(timestamps []int64) {
	for i := range timestamps {
		if i > 0 && timestamps[i-1] > timestamps[i] {
			logger.Panicf("log entries must be sorted by timestamp; got the previous entry with bigger timestamp %d than the current entry with timestamp %d",
				timestamps[i-1], timestamps[i])
		}
	}
}

func (b *block) mustInitFromTags(tagFamilies [][]tagValues) {
	elementsLen := len(tagFamilies)
	if elementsLen == 0 {
		return
	}
	for i, tff := range tagFamilies {
		b.processTagFamilies(tff, i, elementsLen)
	}
}

func (b *block) processTagFamilies(tff []tagValues, i int, elementsLen int) {
	tagFamilies := b.resizeTagFamilies(len(tff))
	for j, tf := range tff {
		tagFamilies[j].name = tf.tag
		b.processTags(tf, j, i, elementsLen)
	}
}

func (b *block) processTags(tf tagValues, tagFamilyIdx, i int, elementsLen int) {
	tags := b.tagFamilies[tagFamilyIdx].resizeTags(len(tf.values))
	for j, t := range tf.values {
		tags[j].name = t.tag
		tags[j].resizeValues(elementsLen)
		tags[j].valueType = t.valueType
		tags[j].values[i] = t.marshal()
	}
}

func (b *block) resizeTagFamilies(tagFamiliesLen int) []tagFamily {
	tff := b.tagFamilies[:0]
	if n := tagFamiliesLen - cap(tff); n > 0 {
		tff = append(tff[:cap(tff)], make([]tagFamily, n)...)
	}
	tff = tff[:tagFamiliesLen]
	b.tagFamilies = tff
	return tff
}

func (b *block) Len() int {
	return len(b.timestamps)
}

func (b *block) mustWriteTo(sid common.SeriesID, bm *blockMetadata, ww *writers) {
	b.validate()
	bm.reset()

	bm.seriesID = sid
	bm.uncompressedSizeBytes = b.uncompressedSizeBytes()
	bm.count = uint64(b.Len())

	mustWriteTimestampsTo(&bm.timestamps, b.timestamps, &ww.timestampsWriter)
	mustWriteElementIDsTo(&bm.elementIDs, b.elementIDs, &ww.elementIDsWriter)

	for ti := range b.tagFamilies {
		b.marshalTagFamily(b.tagFamilies[ti], bm, ww)
	}
}

func (b *block) validate() {
	timestamps := b.timestamps
	for i := 1; i < len(timestamps); i++ {
		if timestamps[i-1] > timestamps[i] {
			logger.Panicf("log entries must be sorted by timestamp; got the previous entry with bigger timestamp %d than the current entry with timestamp %d",
				timestamps[i-1], timestamps[i])
		}
	}

	itemsCount := len(timestamps)
	if itemsCount != len(b.elementIDs) {
		logger.Panicf("unexpected number of values for elementIDs: got %d; want %d", len(b.elementIDs), itemsCount)
	}
	tff := b.tagFamilies
	for _, tf := range tff {
		for _, c := range tf.tags {
			if len(c.values) != itemsCount {
				logger.Panicf("unexpected number of values for tags %q: got %d; want %d", c.name, len(c.values), itemsCount)
			}
		}
	}
}

func (b *block) marshalTagFamily(tf tagFamily, bm *blockMetadata, ww *writers) {
	hw, w := ww.getTagMetadataWriterAndTagWriter(tf.name)
	cc := tf.tags
	cfm := generateTagFamilyMetadata()
	cmm := cfm.resizeTagMetadata(len(cc))
	for i := range cc {
		cc[i].mustWriteTo(&cmm[i], w)
	}
	bb := bigValuePool.Generate()
	defer bigValuePool.Release(bb)
	bb.Buf = cfm.marshal(bb.Buf)
	releaseTagFamilyMetadata(cfm)
	tfm := bm.getTagFamilyMetadata(tf.name)
	tfm.offset = hw.bytesWritten
	tfm.size = uint64(len(bb.Buf))
	if tfm.size > maxTagFamiliesMetadataSize {
		logger.Panicf("too big tagFamilyMetadataSize: %d bytes; mustn't exceed %d bytes", tfm.size, maxTagFamiliesMetadataSize)
	}
	hw.MustWrite(bb.Buf)
}

func (b *block) unmarshalTagFamily(decoder *encoding.BytesBlockDecoder, tfIndex int, name string,
	tagFamilyMetadataBlock *dataBlock, tagProjection []string, metaReader, valueReader fs.Reader,
) {
	if len(tagProjection) < 1 {
		return
	}
	bb := bigValuePool.Generate()
	bb.Buf = bytes.ResizeExact(bb.Buf, int(tagFamilyMetadataBlock.size))
	fs.MustReadData(metaReader, int64(tagFamilyMetadataBlock.offset), bb.Buf)
	tfm := generateTagFamilyMetadata()
	defer releaseTagFamilyMetadata(tfm)
	err := tfm.unmarshal(bb.Buf)
	if err != nil {
		logger.Panicf("%s: cannot unmarshal tagFamilyMetadata: %v", metaReader.Path(), err)
	}
	bigValuePool.Release(bb)
	b.tagFamilies[tfIndex].name = name
	if len(tagProjection) < 1 {
		return
	}
	cc := b.tagFamilies[tfIndex].resizeTags(len(tagProjection))
	for j := range tagProjection {
		for i := range tfm.tagMetadata {
			if tagProjection[j] == tfm.tagMetadata[i].name {
				cc[j].mustReadValues(decoder, valueReader, tfm.tagMetadata[i], uint64(b.Len()))
				break
			}
		}
	}
}

func (b *block) unmarshalTagFamilyFromSeqReaders(decoder *encoding.BytesBlockDecoder, tfIndex int, name string,
	columnFamilyMetadataBlock *dataBlock, metaReader, valueReader *seqReader,
) {
	if columnFamilyMetadataBlock.offset != metaReader.bytesRead {
		logger.Panicf("offset %d must be equal to bytesRead %d", columnFamilyMetadataBlock.offset, metaReader.bytesRead)
	}
	bb := bigValuePool.Generate()
	bb.Buf = bytes.ResizeExact(bb.Buf, int(columnFamilyMetadataBlock.size))
	metaReader.mustReadFull(bb.Buf)
	tfm := generateTagFamilyMetadata()
	defer releaseTagFamilyMetadata(tfm)
	err := tfm.unmarshal(bb.Buf)
	if err != nil {
		logger.Panicf("%s: cannot unmarshal columnFamilyMetadata: %v", metaReader.Path(), err)
	}
	bigValuePool.Release(bb)
	b.tagFamilies[tfIndex].name = name

	cc := b.tagFamilies[tfIndex].resizeTags(len(tfm.tagMetadata))
	for i := range tfm.tagMetadata {
		cc[i].mustSeqReadValues(decoder, valueReader, tfm.tagMetadata[i], uint64(b.Len()))
	}
}

func (b *block) uncompressedSizeBytes() uint64 {
	elementsCount := uint64(b.Len())

	n := elementsCount * 8

	tff := b.tagFamilies
	for i := range tff {
		tf := tff[i]
		nameLen := uint64(len(tf.name))
		for _, c := range tf.tags {
			nameLen += uint64(len(c.name))
			for _, v := range c.values {
				if len(v) > 0 {
					n += nameLen + uint64(len(v))
				}
			}
		}
	}
	return n
}

func (b *block) mustReadFrom(decoder *encoding.BytesBlockDecoder, p *part, bm blockMetadata) {
	b.reset()

	b.timestamps = mustReadTimestampsFrom(b.timestamps, &bm.timestamps, int(bm.count), p.timestamps)
	b.elementIDs = mustReadElementIDsFrom(b.elementIDs, &bm.elementIDs, int(bm.count), p.elementIDs)

	_ = b.resizeTagFamilies(len(bm.tagProjection))
	for i := range bm.tagProjection {
		name := bm.tagProjection[i].Family
		block, ok := bm.tagFamilies[name]
		if !ok {
			continue
		}
		b.unmarshalTagFamily(decoder, i, name, block,
			bm.tagProjection[i].Names, p.tagFamilyMetadata[name],
			p.tagFamilies[name])
	}
}

func (b *block) mustSeqReadFrom(decoder *encoding.BytesBlockDecoder, seqReaders *seqReaders, bm blockMetadata) {
	b.reset()

	b.timestamps = mustSeqReadTimestampsFrom(b.timestamps, &bm.timestamps, int(bm.count), &seqReaders.timestamps)
	b.elementIDs = mustSeqReadElementIDsFrom(b.elementIDs, &bm.elementIDs, int(bm.count), &seqReaders.elementIDs)

	_ = b.resizeTagFamilies(len(bm.tagFamilies))
	keys := make([]string, 0, len(bm.tagFamilies))
	for k := range bm.tagFamilies {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, name := range keys {
		block := bm.tagFamilies[name]
		b.unmarshalTagFamilyFromSeqReaders(decoder, i, name, block,
			seqReaders.tagFamilyMetadata[name], seqReaders.tagFamilies[name])
	}
}

// For testing purpose only.
func (b *block) sortTagFamilies() {
	sort.Slice(b.tagFamilies, func(i, j int) bool {
		return b.tagFamilies[i].name < b.tagFamilies[j].name
	})
}

func mustWriteTimestampsTo(tm *timestampsMetadata, timestamps []int64, timestampsWriter *writer) {
	tm.reset()

	bb := bigValuePool.Generate()
	defer bigValuePool.Release(bb)
	bb.Buf, tm.encodeType, tm.min = encoding.Int64ListToBytes(bb.Buf[:0], timestamps)
	if len(bb.Buf) > maxTimestampsBlockSize {
		logger.Panicf("too big block with timestamps: %d bytes; the maximum supported size is %d bytes", len(bb.Buf), maxTimestampsBlockSize)
	}
	tm.max = timestamps[len(timestamps)-1]
	tm.offset = timestampsWriter.bytesWritten
	tm.size = uint64(len(bb.Buf))
	timestampsWriter.MustWrite(bb.Buf)
}

func mustReadTimestampsFrom(dst []int64, tm *timestampsMetadata, count int, reader fs.Reader) []int64 {
	bb := bigValuePool.Generate()
	defer bigValuePool.Release(bb)
	bb.Buf = bytes.ResizeExact(bb.Buf, int(tm.size))
	fs.MustReadData(reader, int64(tm.offset), bb.Buf)
	var err error
	dst, err = encoding.BytesToInt64List(dst, bb.Buf, tm.encodeType, tm.min, count)
	if err != nil {
		logger.Panicf("%s: cannot unmarshal timestamps: %v", reader.Path(), err)
	}
	return dst
}

func mustWriteElementIDsTo(em *elementIDsMetadata, elementIDs []string, elementIDsWriter *writer) {
	em.reset()

	bb := bigValuePool.Generate()
	defer bigValuePool.Release(bb)
	elementIDsByteSlice := make([][]byte, len(elementIDs))
	for i, elementID := range elementIDs {
		elementIDsByteSlice[i] = []byte(elementID)
	}
	bb.Buf = encoding.EncodeBytesBlock(bb.Buf, elementIDsByteSlice)
	if len(bb.Buf) > maxElementIDsBlockSize {
		logger.Panicf("too big block with elementIDs: %d bytes; the maximum supported size is %d bytes", len(bb.Buf), maxElementIDsBlockSize)
	}
	em.encodeType = encoding.EncodeTypeUnknown
	em.offset = elementIDsWriter.bytesWritten
	em.size = uint64(len(bb.Buf))
	elementIDsWriter.MustWrite(bb.Buf)
}

func mustReadElementIDsFrom(dst []string, em *elementIDsMetadata, count int, reader fs.Reader) []string {
	bb := bigValuePool.Generate()
	defer bigValuePool.Release(bb)
	bb.Buf = bytes.ResizeExact(bb.Buf, int(em.size))
	fs.MustReadData(reader, int64(em.offset), bb.Buf)
	decoder := encoding.BytesBlockDecoder{}
	var elementIDsByteSlice [][]byte
	elementIDsByteSlice, err := decoder.Decode(elementIDsByteSlice, bb.Buf, uint64(count))
	if err != nil {
		logger.Panicf("%s: cannot unmarshal elementIDs: %v", reader.Path(), err)
	}
	for _, elementID := range elementIDsByteSlice {
		dst = append(dst, string(elementID))
	}
	return dst
}

func mustSeqReadTimestampsFrom(dst []int64, tm *timestampsMetadata, count int, reader *seqReader) []int64 {
	if tm.offset != reader.bytesRead {
		logger.Panicf("offset %d must be equal to bytesRead %d", tm.offset, reader.bytesRead)
	}
	bb := bigValuePool.Generate()
	defer bigValuePool.Release(bb)
	bb.Buf = bytes.ResizeExact(bb.Buf, int(tm.size))
	reader.mustReadFull(bb.Buf)
	var err error
	dst, err = encoding.BytesToInt64List(dst, bb.Buf, tm.encodeType, tm.min, count)
	if err != nil {
		logger.Panicf("%s: cannot unmarshal timestamps: %v", reader.Path(), err)
	}
	return dst
}

func mustSeqReadElementIDsFrom(dst []string, em *elementIDsMetadata, count int, reader *seqReader) []string {
	if em.offset != reader.bytesRead {
		logger.Panicf("offset %d must be equal to bytesRead %d", em.offset, reader.bytesRead)
	}
	bb := bigValuePool.Generate()
	defer bigValuePool.Release(bb)
	bb.Buf = bytes.ResizeExact(bb.Buf, int(em.size))
	reader.mustReadFull(bb.Buf)
	decoder := encoding.BytesBlockDecoder{}
	var elementIDsByteSlice [][]byte
	elementIDsByteSlice, err := decoder.Decode(elementIDsByteSlice, bb.Buf, uint64(count))
	if err != nil {
		logger.Panicf("%s: cannot unmarshal elementIDs: %v", reader.Path(), err)
	}
	for _, elementID := range elementIDsByteSlice {
		dst = append(dst, string(elementID))
	}
	return dst
}

func generateBlock() *block {
	v := blockPool.Get()
	if v == nil {
		return &block{}
	}
	return v.(*block)
}

func releaseBlock(b *block) {
	b.reset()
	blockPool.Put(b)
}

var blockPool sync.Pool

type blockCursor struct {
	p                *part
	timestamps       []int64
	elementIDs       []string
	tagFamilies      []tagFamily
	tagValuesDecoder encoding.BytesBlockDecoder
	tagProjection    []pbv1.TagProjection
	bm               blockMetadata
	idx              int
	minTimestamp     int64
	maxTimestamp     int64
}

func (bc *blockCursor) reset() {
	bc.idx = 0
	bc.p = nil
	bc.bm = blockMetadata{}
	bc.minTimestamp = 0
	bc.maxTimestamp = 0
	bc.tagProjection = bc.tagProjection[:0]

	bc.timestamps = bc.timestamps[:0]
	bc.elementIDs = bc.elementIDs[:0]

	tff := bc.tagFamilies
	for i := range tff {
		tff[i].reset()
	}
	bc.tagFamilies = tff[:0]
}

func (bc *blockCursor) init(p *part, bm blockMetadata, queryOpts queryOptions) {
	bc.reset()
	bc.p = p
	bc.bm = bm
	bc.minTimestamp = queryOpts.minTimestamp
	bc.maxTimestamp = queryOpts.maxTimestamp
	bc.tagProjection = queryOpts.TagProjection
}

func (bc *blockCursor) copyAllTo(r *pbv1.StreamResult, desc bool) {
	var idx, offset int
	if desc {
		idx = 0
		offset = bc.idx + 1
	} else {
		idx = bc.idx
		offset = len(bc.timestamps)
	}
	if offset <= idx {
		return
	}
	r.SID = bc.bm.seriesID
	r.Timestamps = append(r.Timestamps, bc.timestamps[idx:offset]...)
	r.ElementIDs = append(r.ElementIDs, bc.elementIDs[idx:offset]...)
	if len(r.TagFamilies) != len(bc.tagProjection) {
		for _, tp := range bc.tagProjection {
			tf := pbv1.TagFamily{
				Name: tp.Family,
			}
			for _, n := range tp.Names {
				t := pbv1.Tag{
					Name: n,
				}
				tf.Tags = append(tf.Tags, t)
			}
			r.TagFamilies = append(r.TagFamilies, tf)
		}
	}
	for i, cf := range bc.tagFamilies {
		for i2, c := range cf.tags {
			if c.values != nil {
				for _, v := range c.values[idx:offset] {
					r.TagFamilies[i].Tags[i2].Values = append(r.TagFamilies[i].Tags[i2].Values, mustDecodeTagValue(c.valueType, v))
				}
			} else {
				for j := idx; j < offset; j++ {
					r.TagFamilies[i].Tags[i2].Values = append(r.TagFamilies[i].Tags[i2].Values, pbv1.NullTagValue)
				}
			}
		}
	}
}

func (bc *blockCursor) copyTo(r *pbv1.StreamResult) {
	r.SID = bc.bm.seriesID
	r.Timestamps = append(r.Timestamps, bc.timestamps[bc.idx])
	r.ElementIDs = append(r.ElementIDs, bc.elementIDs[bc.idx])
	if len(r.TagFamilies) != len(bc.tagProjection) {
		for _, tp := range bc.tagProjection {
			tf := pbv1.TagFamily{
				Name: tp.Family,
			}
			for _, n := range tp.Names {
				t := pbv1.Tag{
					Name: n,
				}
				tf.Tags = append(tf.Tags, t)
			}
			r.TagFamilies = append(r.TagFamilies, tf)
		}
	}
	if len(bc.tagFamilies) != len(r.TagFamilies) {
		logger.Panicf("unexpected number of tag families: got %d; want %d", len(bc.tagFamilies), len(r.TagFamilies))
	}
	for i, cf := range bc.tagFamilies {
		if len(r.TagFamilies[i].Tags) != len(cf.tags) {
			logger.Panicf("unexpected number of tags: got %d; want %d", len(r.TagFamilies[i].Tags), len(bc.tagProjection[i].Names))
		}
		for i2, c := range cf.tags {
			if c.values != nil {
				r.TagFamilies[i].Tags[i2].Values = append(r.TagFamilies[i].Tags[i2].Values, mustDecodeTagValue(c.valueType, c.values[bc.idx]))
			} else {
				r.TagFamilies[i].Tags[i2].Values = append(r.TagFamilies[i].Tags[i2].Values, pbv1.NullTagValue)
			}
		}
	}
}

func (bc *blockCursor) loadData(tmpBlock *block) bool {
	tmpBlock.reset()
	bc.bm.tagProjection = bc.tagProjection
	tf := make(map[string]*dataBlock, len(bc.tagProjection))
	for i := range bc.tagProjection {
		for tfName, block := range bc.bm.tagFamilies {
			if bc.tagProjection[i].Family == tfName {
				tf[tfName] = block
			}
		}
	}
	bc.bm.tagFamilies = tf
	tmpBlock.mustReadFrom(&bc.tagValuesDecoder, bc.p, bc.bm)

	start, end, ok := timestamp.FindRange(tmpBlock.timestamps, bc.minTimestamp, bc.maxTimestamp)
	if !ok {
		return false
	}
	bc.timestamps = append(bc.timestamps, tmpBlock.timestamps[start:end+1]...)
	bc.elementIDs = append(bc.elementIDs, tmpBlock.elementIDs[start:end+1]...)

	for i, projection := range bc.bm.tagProjection {
		tf := tagFamily{
			name: projection.Family,
		}
		blockIndex := 0
		for _, name := range projection.Names {
			t := tag{
				name: name,
			}
			if tmpBlock.tagFamilies[i].tags[blockIndex].name == name {
				t.valueType = tmpBlock.tagFamilies[i].tags[blockIndex].valueType
				if len(tmpBlock.tagFamilies[i].tags[blockIndex].values) != len(tmpBlock.timestamps) {
					logger.Panicf("unexpected number of values for tags %q: got %d; want %d",
						tmpBlock.tagFamilies[i].tags[blockIndex].name, len(tmpBlock.tagFamilies[i].tags[blockIndex].values), len(tmpBlock.timestamps))
				}
				t.values = append(t.values, tmpBlock.tagFamilies[i].tags[blockIndex].values[start:end+1]...)
			}
			blockIndex++
			tf.tags = append(tf.tags, t)
		}
		bc.tagFamilies = append(bc.tagFamilies, tf)
	}
	return true
}

var blockCursorPool sync.Pool

func generateBlockCursor() *blockCursor {
	v := blockCursorPool.Get()
	if v == nil {
		return &blockCursor{}
	}
	return v.(*blockCursor)
}

func releaseBlockCursor(bc *blockCursor) {
	bc.reset()
	blockCursorPool.Put(bc)
}

type blockPointer struct {
	block
	bm  blockMetadata
	idx int
}

func (bi *blockPointer) updateMetadata() {
	if len(bi.block.timestamps) == 0 {
		return
	}
	// only update timestamps since they are used for merging
	// blockWriter will recompute all fields
	bi.bm.timestamps.min = bi.block.timestamps[0]
	bi.bm.timestamps.max = bi.block.timestamps[len(bi.timestamps)-1]
}

func (bi *blockPointer) copyFrom(src *blockPointer) {
	bi.idx = 0
	bi.bm.copyFrom(&src.bm)
	bi.appendAll(src)
}

func (bi *blockPointer) appendAll(b *blockPointer) {
	if len(b.timestamps) == 0 {
		return
	}
	bi.append(b, len(b.timestamps))
}

func (bi *blockPointer) append(b *blockPointer, offset int) {
	if offset <= b.idx {
		return
	}
	if len(bi.tagFamilies) == 0 && len(b.tagFamilies) > 0 {
		for _, tf := range b.tagFamilies {
			tFamily := tagFamily{name: tf.name}
			for _, c := range tf.tags {
				col := tag{name: c.name, valueType: c.valueType}
				assertIdxAndOffset(col.name, len(c.values), b.idx, offset)
				col.values = append(col.values, c.values[b.idx:offset]...)
				tFamily.tags = append(tFamily.tags, col)
			}
			bi.tagFamilies = append(bi.tagFamilies, tFamily)
		}
	} else {
		if len(bi.tagFamilies) != len(b.tagFamilies) {
			logger.Panicf("unexpected number of tag families: got %d; want %d", len(bi.tagFamilies), len(b.tagFamilies))
		}
		for i := range bi.tagFamilies {
			if bi.tagFamilies[i].name != b.tagFamilies[i].name {
				logger.Panicf("unexpected tag family name: got %q; want %q", bi.tagFamilies[i].name, b.tagFamilies[i].name)
			}
			if len(bi.tagFamilies[i].tags) != len(b.tagFamilies[i].tags) {
				logger.Panicf("unexpected number of tags for tag family %q: got %d; want %d", bi.tagFamilies[i].name, len(bi.tagFamilies[i].tags), len(b.tagFamilies[i].tags))
			}
			for j := range bi.tagFamilies[i].tags {
				if bi.tagFamilies[i].tags[j].name != b.tagFamilies[i].tags[j].name {
					logger.Panicf("unexpected tag name for tag family %q: got %q; want %q", bi.tagFamilies[i].name, bi.tagFamilies[i].tags[j].name, b.tagFamilies[i].tags[j].name)
				}
				assertIdxAndOffset(b.tagFamilies[i].tags[j].name, len(b.tagFamilies[i].tags[j].values), b.idx, offset)
				bi.tagFamilies[i].tags[j].values = append(bi.tagFamilies[i].tags[j].values, b.tagFamilies[i].tags[j].values[b.idx:offset]...)
			}
		}
	}

	assertIdxAndOffset("timestamps", len(b.timestamps), bi.idx, offset)
	bi.timestamps = append(bi.timestamps, b.timestamps[b.idx:offset]...)
	bi.elementIDs = append(bi.elementIDs, b.elementIDs[b.idx:offset]...)
}

func assertIdxAndOffset(name string, length int, idx int, offset int) {
	if idx >= offset {
		logger.Panicf("%q idx %d must be less than offset %d", name, idx, offset)
	}
	if offset > length {
		logger.Panicf("%q offset %d must be less than or equal to length %d", name, offset, length)
	}
}

func (bi *blockPointer) isFull() bool {
	return bi.bm.uncompressedSizeBytes >= maxUncompressedBlockSize
}

func (bi *blockPointer) reset() {
	bi.idx = 0
	bi.block.reset()
	bi.bm = blockMetadata{}
}

func generateBlockPointer() *blockPointer {
	v := blockPointerPool.Get()
	if v == nil {
		return &blockPointer{}
	}
	return v.(*blockPointer)
}

func releaseBlockPointer(bi *blockPointer) {
	bi.reset()
	blockPointerPool.Put(bi)
}

var blockPointerPool sync.Pool
