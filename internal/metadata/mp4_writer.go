package metadata

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/zhaarey/go-mp4tag"
)

type rawMP4Box struct {
	typ        string
	start      int
	end        int
	headerSize int
	raw        []byte
}

type topLevelMP4Box struct {
	typ   string
	start int64
	end   int64
}

func parseRawMP4Boxes(data []byte, start, end int) ([]rawMP4Box, error) {
	if start < 0 || end < start || end > len(data) {
		return nil, errors.New("invalid MP4 box range")
	}

	var boxes []rawMP4Box
	for offset := start; offset < end; {
		if end-offset < 8 {
			return nil, errors.New("truncated MP4 box header")
		}

		size32 := binary.BigEndian.Uint32(data[offset : offset+4])
		typ := string(data[offset+4 : offset+8])
		headerSize := 8
		var boxSize uint64

		switch size32 {
		case 0:
			boxSize = uint64(end - offset)
		case 1:
			if end-offset < 16 {
				return nil, errors.New("truncated large MP4 box header")
			}
			boxSize = binary.BigEndian.Uint64(data[offset+8 : offset+16])
			headerSize = 16
		default:
			boxSize = uint64(size32)
		}

		if boxSize < uint64(headerSize) || boxSize > uint64(end-offset) {
			return nil, fmt.Errorf("invalid %s box size %d", typ, boxSize)
		}

		boxEnd := offset + int(boxSize)
		boxes = append(boxes, rawMP4Box{
			typ:        typ,
			start:      offset,
			end:        boxEnd,
			headerSize: headerSize,
			raw:        data[offset:boxEnd],
		})
		offset = boxEnd
	}

	return boxes, nil
}

func encodeRawMP4Box(typ string, payload []byte, preferredHeaderSize int) ([]byte, error) {
	if len(typ) != 4 {
		return nil, fmt.Errorf("MP4 box type must be four bytes: %q", typ)
	}

	headerSize := preferredHeaderSize
	if headerSize != 8 && headerSize != 16 {
		headerSize = 8
	}
	boxSize := uint64(headerSize + len(payload))
	if boxSize >= 1<<32 {
		headerSize = 16
		boxSize = uint64(headerSize + len(payload))
	}

	out := make([]byte, headerSize+len(payload))
	if headerSize == 16 {
		binary.BigEndian.PutUint32(out[:4], 1)
		copy(out[4:8], typ)
		binary.BigEndian.PutUint64(out[8:16], boxSize)
	} else {
		binary.BigEndian.PutUint32(out[:4], uint32(boxSize))
		copy(out[4:8], typ)
	}
	copy(out[headerSize:], payload)
	return out, nil
}

func rawBoxPayload(box rawMP4Box) []byte {
	return box.raw[box.headerSize:]
}

func findRawMP4Box(boxes []rawMP4Box, typ string) *rawMP4Box {
	for index := range boxes {
		if boxes[index].typ == typ {
			return &boxes[index]
		}
	}
	return nil
}

func joinRawMP4Boxes(boxes ...[]byte) []byte {
	var size int
	for _, box := range boxes {
		size += len(box)
	}
	out := make([]byte, 0, size)
	for _, box := range boxes {
		out = append(out, box...)
	}
	return out
}

func detectMetaPrefix(payload []byte) int {
	if len(payload) >= 12 {
		if _, err := parseRawMP4Boxes(payload, 4, len(payload)); err == nil {
			return 4
		}
	}
	return 0
}

func parseContainerChildren(box rawMP4Box, prefixSize int) ([]byte, []rawMP4Box, error) {
	payload := rawBoxPayload(box)
	if prefixSize < 0 || prefixSize > len(payload) {
		return nil, nil, errors.New("invalid MP4 container prefix")
	}
	children, err := parseRawMP4Boxes(payload, prefixSize, len(payload))
	if err != nil {
		return nil, nil, err
	}
	prefix := append([]byte(nil), payload[:prefixSize]...)
	return prefix, children, nil
}

func mp4HandlerBox() ([]byte, error) {
	payload := make([]byte, 0, 29)
	payload = append(payload, 0, 0, 0, 0) // version + flags
	payload = append(payload, 0, 0, 0, 0) // pre-defined
	payload = append(payload, []byte("mdir")...)
	payload = append(payload, make([]byte, 12)...)
	payload = append(payload, []byte("appl")...)
	payload = append(payload, 0)
	return encodeRawMP4Box("hdlr", payload, 8)
}

func mp4DataBox(dataType uint32, value []byte) ([]byte, error) {
	payload := make([]byte, 8+len(value))
	binary.BigEndian.PutUint32(payload[:4], dataType)
	copy(payload[8:], value)
	return encodeRawMP4Box("data", payload, 8)
}

func mp4TextItem(boxName string, value string, copyrightPrefix bool) ([]byte, error) {
	itemType := boxName
	if copyrightPrefix {
		itemType = string([]byte{0xA9}) + boxName
	}
	data, err := mp4DataBox(1, []byte(value))
	if err != nil {
		return nil, err
	}
	return encodeRawMP4Box(itemType, data, 8)
}

func mp4NumericItem(boxName string, payload []byte) ([]byte, error) {
	data, err := encodeRawMP4Box("data", payload, 8)
	if err != nil {
		return nil, err
	}
	return encodeRawMP4Box(boxName, data, 8)
}

func mp4TrackOrDiscItem(trackNumber, total int16, track bool) ([]byte, error) {
	if track {
		payload := make([]byte, 16)
		binary.BigEndian.PutUint16(payload[10:12], uint16(maxInt16(trackNumber)))
		binary.BigEndian.PutUint16(payload[12:14], uint16(maxInt16(total)))
		return mp4NumericItem("trkn", payload)
	}

	payload := make([]byte, 14)
	binary.BigEndian.PutUint16(payload[10:12], uint16(maxInt16(trackNumber)))
	binary.BigEndian.PutUint16(payload[12:14], uint16(maxInt16(total)))
	return mp4NumericItem("disk", payload)
}

func mp4AdvisoryItem(advisory mp4tag.ItunesAdvisory) ([]byte, error) {
	payload := make([]byte, 9)
	binary.BigEndian.PutUint32(payload[:4], 0x15)
	payload[8] = byte(advisory)
	return mp4NumericItem("rtng", payload)
}

func mp4BPMItem(bpm int16) ([]byte, error) {
	payload := make([]byte, 10)
	binary.BigEndian.PutUint32(payload[:4], 0x15)
	binary.BigEndian.PutUint16(payload[8:10], uint16(maxInt16(bpm)))
	return mp4NumericItem("tmpo", payload)
}

func mp4GenreItem(genre mp4tag.Genre) ([]byte, error) {
	payload := make([]byte, 10)
	payload[9] = byte(genre)
	return mp4NumericItem("gnre", payload)
}

func mp4IDItem(boxName string, id int32, longPayload bool) ([]byte, error) {
	payloadSize := 12
	idOffset := 8
	if longPayload {
		payloadSize = 16
		idOffset = 12
	}
	payload := make([]byte, payloadSize)
	binary.BigEndian.PutUint32(payload[:4], 0x15)
	binary.BigEndian.PutUint32(payload[idOffset:idOffset+4], uint32(id))
	return mp4NumericItem(boxName, payload)
}

func mp4CustomItem(name, value string, otherValues []string) ([]byte, error) {
	meanPayload := append(make([]byte, 4), []byte("com.apple.iTunes")...)
	mean, err := encodeRawMP4Box("mean", meanPayload, 8)
	if err != nil {
		return nil, err
	}

	namePayload := append(make([]byte, 4), []byte(name)...)
	nameBox, err := encodeRawMP4Box("name", namePayload, 8)
	if err != nil {
		return nil, err
	}

	data, err := mp4DataBox(1, []byte(value))
	if err != nil {
		return nil, err
	}
	parts := [][]byte{mean, nameBox, data}
	for _, other := range otherValues {
		otherData, err := mp4DataBox(1, []byte(other))
		if err != nil {
			return nil, err
		}
		parts = append(parts, otherData)
	}
	return encodeRawMP4Box("----", joinRawMP4Boxes(parts...), 8)
}

func mp4PictureItem(pictures []*mp4tag.MP4Picture) ([]byte, error) {
	var dataBoxes [][]byte
	for _, picture := range pictures {
		if picture == nil || len(picture.Data) < 1 {
			continue
		}
		isJPEG := len(picture.Data) >= 3 &&
			bytes.Equal(picture.Data[:3], []byte{0xFF, 0xD8, 0xFF})
		isPNG := len(picture.Data) >= 8 &&
			bytes.Equal(
				picture.Data[:8],
				[]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A},
			)

		var format uint32
		switch picture.Format {
		case mp4tag.ImageTypeJPEG:
			if !isJPEG {
				return nil, errors.New(
					"JPEG cover type does not match artwork bytes",
				)
			}
			format = 13
		case mp4tag.ImageTypePNG:
			if !isPNG {
				return nil, errors.New(
					"PNG cover type does not match artwork bytes",
				)
			}
			format = 14
		case mp4tag.ImageTypeAuto:
			switch {
			case isJPEG:
				format = 13
			case isPNG:
				format = 14
			default:
				return nil, errors.New(
					"cover artwork is not a JPEG or PNG image",
				)
			}
		default:
			return nil, errors.New("unsupported MP4 cover image type")
		}
		data, err := mp4DataBox(format, picture.Data)
		if err != nil {
			return nil, err
		}
		dataBoxes = append(dataBoxes, data)
	}
	if len(dataBoxes) == 0 {
		return nil, nil
	}
	return encodeRawMP4Box("covr", joinRawMP4Boxes(dataBoxes...), 8)
}

func mp4KnownItemTypes() map[string]bool {
	return map[string]bool{
		string([]byte{0xA9, 'n', 'a', 'm'}): true,
		"sonm":                              true,
		"alb":                               true,
		"soal":                              true,
		"aART":                              true,
		"soaa":                              true,
		string([]byte{0xA9, 'A', 'R', 'T'}): true,
		"soar":                              true,
		string([]byte{0xA9, 'c', 'm', 't'}): true,
		string([]byte{0xA9, 'w', 'r', 't'}): true,
		"soco":                              true,
		"cprt":                              true,
		string([]byte{0xA9, 'l', 'y', 'r'}): true,
		string([]byte{0xA9, 'g', 'e', 'n'}): true,
		"desc":                              true,
		string([]byte{0xA9, 'p', 'u', 'b'}): true,
		"con":                               true,
		"dir":                               true,
		"nrt":                               true,
		"rtng":                              true,
		"plID":                              true,
		"atID":                              true,
		"trkn":                              true,
		"disk":                              true,
		"tmpo":                              true,
		"gnre":                              true,
		string([]byte{0xA9, 'd', 'a', 'y'}): true,
		"covr":                              true,
		"----":                              true,
	}
}

func buildMP4Ilst(tags *mp4tag.MP4Tags, oldChildren []rawMP4Box) ([]byte, error) {
	var items [][]byte
	appendText := func(name, value string, copyrightPrefix bool) error {
		if value == "" {
			return nil
		}
		item, err := mp4TextItem(name, value, copyrightPrefix)
		if err != nil {
			return err
		}
		items = append(items, item)
		return nil
	}

	if err := appendText("nam", tags.Title, true); err != nil {
		return nil, err
	}
	if err := appendText("sonm", tags.TitleSort, false); err != nil {
		return nil, err
	}
	if err := appendText("alb", tags.Album, true); err != nil {
		return nil, err
	}
	if err := appendText("soal", tags.AlbumSort, false); err != nil {
		return nil, err
	}
	if err := appendText("aART", tags.AlbumArtist, false); err != nil {
		return nil, err
	}
	if err := appendText("soaa", tags.AlbumArtistSort, false); err != nil {
		return nil, err
	}
	if err := appendText("ART", tags.Artist, true); err != nil {
		return nil, err
	}
	if err := appendText("soar", tags.ArtistSort, false); err != nil {
		return nil, err
	}
	if err := appendText("cmt", tags.Comment, true); err != nil {
		return nil, err
	}
	if err := appendText("wrt", tags.Composer, true); err != nil {
		return nil, err
	}
	if err := appendText("soco", tags.ComposerSort, false); err != nil {
		return nil, err
	}
	if err := appendText("cprt", tags.Copyright, false); err != nil {
		return nil, err
	}
	if err := appendText("lyr", tags.Lyrics, true); err != nil {
		return nil, err
	}
	if err := appendText("gen", tags.CustomGenre, true); err != nil {
		return nil, err
	}
	if err := appendText("desc", tags.Description, false); err != nil {
		return nil, err
	}
	if err := appendText("pub", tags.Publisher, true); err != nil {
		return nil, err
	}
	if tags.Conductor != "" {
		if err := appendText("con", tags.Conductor, true); err != nil {
			return nil, err
		}
	}
	if tags.Director != "" {
		if err := appendText("dir", tags.Director, false); err != nil {
			return nil, err
		}
	}
	if tags.Narrator != "" {
		if err := appendText("nrt", tags.Narrator, false); err != nil {
			return nil, err
		}
	}
	if tags.BPM > 0 {
		item, err := mp4BPMItem(tags.BPM)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if tags.ItunesAdvisory != mp4tag.ItunesAdvisoryNone {
		item, err := mp4AdvisoryItem(tags.ItunesAdvisory)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if tags.ItunesAlbumID > 0 {
		item, err := mp4IDItem("plID", tags.ItunesAlbumID, true)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if tags.ItunesArtistID > 0 {
		item, err := mp4IDItem("atID", tags.ItunesArtistID, false)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if tags.TrackNumber > 0 || tags.TrackTotal > 0 {
		item, err := mp4TrackOrDiscItem(tags.TrackNumber, tags.TrackTotal, true)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if tags.DiscNumber > 0 || tags.DiscTotal > 0 {
		item, err := mp4TrackOrDiscItem(tags.DiscNumber, tags.DiscTotal, false)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if tags.Year > 0 {
		item, err := mp4TextItem("day", fmt.Sprintf("%d", tags.Year), true)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	} else if tags.Date != "" {
		if err := appendText("day", tags.Date, true); err != nil {
			return nil, err
		}
	}
	if tags.Genre != mp4tag.GenreNone {
		item, err := mp4GenreItem(tags.Genre)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}

	customKeys := make([]string, 0, len(tags.Custom))
	for key := range tags.Custom {
		customKeys = append(customKeys, key)
	}
	sort.Strings(customKeys)
	for _, key := range customKeys {
		if tags.Custom[key] == "" {
			continue
		}
		item, err := mp4CustomItem(key, tags.Custom[key], tags.OtherCustom[key])
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}

	if len(tags.Pictures) > 0 {
		pictures, err := mp4PictureItem(tags.Pictures)
		if err != nil {
			return nil, err
		}
		if pictures != nil {
			items = append(items, pictures)
		}
	}

	known := mp4KnownItemTypes()
	for _, oldChild := range oldChildren {
		if known[oldChild.typ] {
			if oldChild.typ == "covr" && len(tags.Pictures) == 0 {
				items = append(items, append([]byte(nil), oldChild.raw...))
			}
			continue
		}
		items = append(items, append([]byte(nil), oldChild.raw...))
	}

	return encodeRawMP4Box("ilst", joinRawMP4Boxes(items...), 8)
}

func buildMP4Meta(existing *rawMP4Box, ilst []byte) ([]byte, error) {
	if existing == nil {
		handler, err := mp4HandlerBox()
		if err != nil {
			return nil, err
		}
		payload := joinRawMP4Boxes([]byte{0, 0, 0, 0}, handler, ilst)
		return encodeRawMP4Box("meta", payload, 8)
	}

	prefixSize := detectMetaPrefix(rawBoxPayload(*existing))
	prefix, children, err := parseContainerChildren(*existing, prefixSize)
	if err != nil {
		return nil, fmt.Errorf("parse existing meta box: %w", err)
	}

	hasHandler := false
	var preserved [][]byte
	for _, child := range children {
		if child.typ == "ilst" {
			continue
		}
		if child.typ == "hdlr" {
			hasHandler = true
		}
		preserved = append(preserved, child.raw)
	}
	if !hasHandler {
		handler, err := mp4HandlerBox()
		if err != nil {
			return nil, err
		}
		preserved = append([][]byte{handler}, preserved...)
	}
	preserved = append(preserved, ilst)
	return encodeRawMP4Box("meta", append(prefix, joinRawMP4Boxes(preserved...)...), existing.headerSize)
}

func buildMP4Udta(existing *rawMP4Box, meta []byte) ([]byte, error) {
	if existing == nil {
		return encodeRawMP4Box("udta", meta, 8)
	}

	_, children, err := parseContainerChildren(*existing, 0)
	if err != nil {
		return nil, fmt.Errorf("parse existing udta box: %w", err)
	}
	var preserved [][]byte
	for _, child := range children {
		if child.typ != "meta" {
			preserved = append(preserved, child.raw)
		}
	}
	preserved = append(preserved, meta)
	return encodeRawMP4Box("udta", joinRawMP4Boxes(preserved...), existing.headerSize)
}

func buildMP4Moov(original []byte, tags *mp4tag.MP4Tags) ([]byte, error) {
	boxes, err := parseRawMP4Boxes(original, 0, len(original))
	if err != nil {
		return nil, fmt.Errorf("parse moov box: %w", err)
	}
	if len(boxes) != 1 || boxes[0].typ != "moov" {
		return nil, errors.New("metadata writer expected one moov box")
	}
	moov := boxes[0]

	_, moovChildren, err := parseContainerChildren(moov, 0)
	if err != nil {
		return nil, fmt.Errorf("parse moov children: %w", err)
	}

	var udta *rawMP4Box
	for index := range moovChildren {
		if moovChildren[index].typ == "udta" {
			udta = &moovChildren[index]
			break
		}
	}

	var oldIlstChildren []rawMP4Box
	var existingMeta *rawMP4Box
	if udta != nil {
		_, udtaChildren, err := parseContainerChildren(*udta, 0)
		if err != nil {
			return nil, fmt.Errorf("parse udta children: %w", err)
		}
		for index := range udtaChildren {
			if udtaChildren[index].typ != "meta" {
				continue
			}
			existingMeta = &udtaChildren[index]
			prefixSize := detectMetaPrefix(rawBoxPayload(*existingMeta))
			_, metaChildren, err := parseContainerChildren(*existingMeta, prefixSize)
			if err != nil {
				return nil, fmt.Errorf("parse meta children: %w", err)
			}
			for childIndex := range metaChildren {
				if metaChildren[childIndex].typ == "ilst" {
					_, oldIlstChildren, err = parseContainerChildren(metaChildren[childIndex], 0)
					if err != nil {
						return nil, fmt.Errorf("parse ilst children: %w", err)
					}
					break
				}
			}
			break
		}
	}

	ilst, err := buildMP4Ilst(tags, oldIlstChildren)
	if err != nil {
		return nil, err
	}
	meta, err := buildMP4Meta(existingMeta, ilst)
	if err != nil {
		return nil, err
	}
	udtaBytes, err := buildMP4Udta(udta, meta)
	if err != nil {
		return nil, err
	}

	var moovParts [][]byte
	for _, child := range moovChildren {
		if child.typ != "udta" {
			moovParts = append(moovParts, child.raw)
		}
	}
	moovParts = append(moovParts, udtaBytes)
	return encodeRawMP4Box("moov", joinRawMP4Boxes(moovParts...), moov.headerSize)
}

func readTopLevelMP4Header(file *os.File, offset, fileSize int64) (string, int64, error) {
	var header [16]byte
	if _, err := file.ReadAt(header[:8], offset); err != nil {
		return "", 0, err
	}
	size32 := binary.BigEndian.Uint32(header[:4])
	typ := string(header[4:8])
	headerSize := int64(8)
	var boxSize uint64
	switch size32 {
	case 0:
		boxSize = uint64(fileSize - offset)
	case 1:
		if _, err := file.ReadAt(header[8:16], offset+8); err != nil {
			return "", 0, err
		}
		boxSize = binary.BigEndian.Uint64(header[8:16])
		headerSize = 16
	default:
		boxSize = uint64(size32)
	}
	if boxSize < uint64(headerSize) || boxSize > uint64(fileSize-offset) {
		return "", 0, fmt.Errorf("invalid top-level %s box size %d", typ, boxSize)
	}
	return typ, int64(boxSize), nil
}

func scanTopLevelMP4(file *os.File, fileSize int64) ([]topLevelMP4Box, int64, []byte, error) {
	var boxes []topLevelMP4Box
	var moovStart, moovEnd int64 = -1, -1
	var moovBytes []byte
	for offset := int64(0); offset < fileSize; {
		typ, boxSize, err := readTopLevelMP4Header(file, offset, fileSize)
		if err != nil {
			return nil, 0, nil, err
		}
		end := offset + boxSize
		boxes = append(boxes, topLevelMP4Box{typ: typ, start: offset, end: end})
		if typ == "moov" {
			if moovStart >= 0 {
				return nil, 0, nil, errors.New("multiple moov boxes are not supported")
			}
			if boxSize > int64(^uint(0)>>1) {
				return nil, 0, nil, errors.New("moov box is too large")
			}
			moovStart, moovEnd = offset, end
			moovBytes = make([]byte, int(boxSize))
			if _, err := file.ReadAt(moovBytes, offset); err != nil {
				return nil, 0, nil, err
			}
		}
		offset = end
	}
	if moovStart < 0 {
		return nil, 0, nil, errors.New("moov box not present")
	}
	return boxes, moovEnd - moovStart, moovBytes, nil
}

func isRawMP4Container(typ string) bool {
	switch typ {
	case "moov", "trak", "mdia", "minf", "stbl", "edts", "dinf", "udta", "meta", "ilst":
		return true
	default:
		return false
	}
}

func adjustProgressiveChunkOffsets(data []byte, delta, mediaStart int64) error {
	var walk func(start, end int, metaPrefix int) error
	walk = func(start, end int, _ int) error {
		boxes, err := parseRawMP4Boxes(data, start, end)
		if err != nil {
			return err
		}
		for _, box := range boxes {
			payloadStart := box.start + box.headerSize
			payload := data[payloadStart:box.end]
			switch box.typ {
			case "stco":
				if len(payload) < 8 {
					return errors.New("truncated stco box")
				}
				count := binary.BigEndian.Uint32(payload[4:8])
				if uint64(8)+uint64(count)*4 > uint64(len(payload)) {
					return errors.New("invalid stco entry count")
				}
				for index := uint32(0); index < count; index++ {
					position := 8 + int(index)*4
					offset := int64(binary.BigEndian.Uint32(payload[position : position+4]))
					if offset < mediaStart {
						continue
					}
					updated := offset + delta
					if updated < 0 || updated > int64(^uint32(0)) {
						return errors.New("stco offset overflow")
					}
					binary.BigEndian.PutUint32(payload[position:position+4], uint32(updated))
				}
			case "co64":
				if len(payload) < 8 {
					return errors.New("truncated co64 box")
				}
				count := binary.BigEndian.Uint32(payload[4:8])
				if uint64(8)+uint64(count)*8 > uint64(len(payload)) {
					return errors.New("invalid co64 entry count")
				}
				for index := uint32(0); index < count; index++ {
					position := 8 + int(index)*8
					offset := int64(binary.BigEndian.Uint64(payload[position : position+8]))
					if offset < mediaStart {
						continue
					}
					updated := offset + delta
					if updated < 0 {
						return errors.New("co64 offset underflow")
					}
					binary.BigEndian.PutUint64(payload[position:position+8], uint64(updated))
				}
			}

			if !isRawMP4Container(box.typ) {
				continue
			}
			childStart := payloadStart
			if box.typ == "meta" {
				childStart += detectMetaPrefix(payload)
			}
			if childStart < box.end {
				if err := walk(childStart, box.end, 0); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(0, len(data), 0)
}

func writeMP4TagsOnePass(trackPath string, tags *mp4tag.MP4Tags) error {
	input, err := os.Open(trackPath)
	if err != nil {
		return err
	}
	defer input.Close()

	stat, err := input.Stat()
	if err != nil {
		return err
	}
	topLevel, oldMoovSize, oldMoov, err := scanTopLevelMP4(input, stat.Size())
	if err != nil {
		return err
	}

	var moovStart, moovEnd, mediaStart int64 = -1, -1, -1
	hasMoof := false
	for _, box := range topLevel {
		switch box.typ {
		case "moov":
			moovStart, moovEnd = box.start, box.end
		case "moof":
			hasMoof = true
		case "mdat":
			if mediaStart < 0 {
				mediaStart = box.start
			}
		}
	}
	if moovStart < 0 || moovEnd < 0 {
		return errors.New("moov box not present")
	}

	newMoov, err := buildMP4Moov(oldMoov, tags)
	if err != nil {
		return err
	}

	delta := int64(len(newMoov)) - oldMoovSize
	if delta != 0 && !hasMoof && mediaStart > moovStart {
		if err := adjustProgressiveChunkOffsets(newMoov, delta, mediaStart); err != nil {
			return err
		}
	}

	temp, err := os.CreateTemp(filepath.Dir(trackPath), ".i5-metadata-*.m4a")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)

	if _, err := input.Seek(0, io.SeekStart); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := io.CopyN(temp, input, moovStart); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(newMoov); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := input.Seek(moovEnd, io.SeekStart); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := io.Copy(temp, input); err != nil {
		_ = temp.Close()
		return err
	}
	if err := input.Close(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, trackPath); err != nil {
		return err
	}
	return nil
}

func maxInt16(value int16) int16 {
	if value < 0 {
		return 0
	}
	return value
}
