package metadata

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
)

// ALACSampleDescription records the bit depth advertised by an ALAC sample
// entry before MP4Box finalizes the fragmented source.
//
// The value comes from the ALACSpecificConfig ("alac" atom), not from a
// hardcoded Apple Music quality assumption.
type ALACSampleDescription struct {
	TrackID    uint32
	EntryIndex int
	BitDepth   uint8
}

type alacSampleDescriptionLocation struct {
	ALACSampleDescription
	sampleSizeOffset       int
	configSampleSizeOffset int
}

func readMP4Moov(path string) ([]byte, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}
	topLevel, _, moov, err := scanTopLevelMP4(file, info.Size())
	if err != nil {
		return nil, 0, err
	}
	for _, box := range topLevel {
		if box.typ == "moov" {
			return moov, box.start, nil
		}
	}
	return nil, 0, errors.New("moov box not present")
}

func absoluteContainerChildren(
	data []byte,
	box rawMP4Box,
	prefixSize int,
) ([]rawMP4Box, error) {
	payload := rawBoxPayload(box)
	children, err := parseRawMP4Boxes(payload, prefixSize, len(payload))
	if err != nil {
		return nil, err
	}
	base := box.start + box.headerSize
	for index := range children {
		start := children[index].start + base
		end := children[index].end + base
		if start < 0 || end < start || end > len(data) {
			return nil, errors.New("MP4 child box is outside its container")
		}
		children[index].start = start
		children[index].end = end
		children[index].raw = data[start:end]
	}
	return children, nil
}

func trackIDFromTKHD(tkhd rawMP4Box) uint32 {
	body := rawBoxPayload(tkhd)
	if len(body) < 16 {
		return 0
	}
	switch body[0] {
	case 0:
		return binary.BigEndian.Uint32(body[12:16])
	case 1:
		if len(body) < 24 {
			return 0
		}
		return binary.BigEndian.Uint32(body[20:24])
	default:
		return 0
	}
}

func trackIDFromTRAK(data []byte, trak rawMP4Box) uint32 {
	children, err := absoluteContainerChildren(data, trak, 0)
	if err != nil {
		return 0
	}
	tkhd := findRawMP4Box(children, "tkhd")
	if tkhd == nil {
		return 0
	}
	return trackIDFromTKHD(*tkhd)
}

func nestedALACConfig(
	data []byte,
	children []rawMP4Box,
) (rawMP4Box, bool, error) {
	for _, child := range children {
		if child.typ == "alac" {
			return child, true, nil
		}
		switch child.typ {
		case "wave", "sinf", "schi", "rinf":
			nested, err := absoluteContainerChildren(data, child, 0)
			if err != nil {
				return rawMP4Box{}, false, err
			}
			if config, ok, err := nestedALACConfig(data, nested); err != nil {
				return rawMP4Box{}, false, err
			} else if ok {
				return config, true, nil
			}
		}
	}
	return rawMP4Box{}, false, nil
}

func alacConfigInSampleEntry(
	data []byte,
	entry rawMP4Box,
) (rawMP4Box, bool, error) {
	const audioSampleEntryHeaderSize = 28
	childStart := entry.start + entry.headerSize + audioSampleEntryHeaderSize
	if childStart > entry.end {
		return rawMP4Box{}, false, errors.New("truncated audio sample entry")
	}
	children, err := parseRawMP4Boxes(data, childStart, entry.end)
	if err != nil {
		return rawMP4Box{}, false, err
	}
	return nestedALACConfig(data, children)
}

func locateALACSampleDescriptions(
	moov []byte,
) ([]alacSampleDescriptionLocation, error) {
	boxes, err := parseRawMP4Boxes(moov, 0, len(moov))
	if err != nil {
		return nil, err
	}
	if len(boxes) != 1 || boxes[0].typ != "moov" {
		return nil, errors.New("metadata writer expected one moov box")
	}

	moovChildren, err := absoluteContainerChildren(moov, boxes[0], 0)
	if err != nil {
		return nil, err
	}

	locations := make([]alacSampleDescriptionLocation, 0)
	for _, trak := range moovChildren {
		if trak.typ != "trak" {
			continue
		}
		trackID := trackIDFromTRAK(moov, trak)
		trakChildren, err := absoluteContainerChildren(moov, trak, 0)
		if err != nil {
			return nil, err
		}
		mdia := findRawMP4Box(trakChildren, "mdia")
		if mdia == nil {
			continue
		}
		mdiaChildren, err := absoluteContainerChildren(moov, *mdia, 0)
		if err != nil {
			return nil, err
		}
		hdlr := findRawMP4Box(mdiaChildren, "hdlr")
		if hdlr == nil {
			continue
		}
		handlerPayload := rawBoxPayload(*hdlr)
		if len(handlerPayload) < 12 ||
			string(handlerPayload[8:12]) != "soun" {
			continue
		}
		minf := findRawMP4Box(mdiaChildren, "minf")
		if minf == nil {
			continue
		}
		minfChildren, err := absoluteContainerChildren(moov, *minf, 0)
		if err != nil {
			return nil, err
		}
		stbl := findRawMP4Box(minfChildren, "stbl")
		if stbl == nil {
			continue
		}
		stblChildren, err := absoluteContainerChildren(moov, *stbl, 0)
		if err != nil {
			return nil, err
		}
		stsd := findRawMP4Box(stblChildren, "stsd")
		if stsd == nil {
			continue
		}
		stsdPayload := rawBoxPayload(*stsd)
		if len(stsdPayload) < 8 {
			return nil, errors.New("truncated stsd box")
		}
		entries, err := parseRawMP4Boxes(
			moov,
			stsd.start+stsd.headerSize+8,
			stsd.end,
		)
		if err != nil {
			return nil, err
		}
		for entryIndex, entry := range entries {
			if entry.typ != "alac" &&
				entry.typ != "enca" &&
				entry.typ != "mp4a" {
				continue
			}
			config, ok, err := alacConfigInSampleEntry(moov, entry)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			configPayload := rawBoxPayload(config)
			if len(configPayload) < 10 {
				return nil, errors.New("truncated ALACSpecificConfig")
			}
			bitDepth := configPayload[9]
			if bitDepth == 0 || bitDepth > 32 {
				return nil, fmt.Errorf(
					"invalid ALAC bit depth %d",
					bitDepth,
				)
			}
			sampleSizeOffset := entry.start + entry.headerSize + 18
			if sampleSizeOffset+2 > entry.end {
				return nil, errors.New("truncated ALAC sample entry")
			}
			locations = append(locations, alacSampleDescriptionLocation{
				ALACSampleDescription: ALACSampleDescription{
					TrackID:    trackID,
					EntryIndex: entryIndex,
					BitDepth:   bitDepth,
				},
				sampleSizeOffset: sampleSizeOffset,
				configSampleSizeOffset: config.start +
					config.headerSize +
					9,
			})
		}
	}
	return locations, nil
}

// CaptureALACSampleDescriptions reads only the MP4 moov box and records the
// source ALAC bit depth. It does not read or rewrite media payload bytes.
func CaptureALACSampleDescriptions(
	path string,
) ([]ALACSampleDescription, error) {
	moov, _, err := readMP4Moov(path)
	if err != nil {
		return nil, err
	}
	locations, err := locateALACSampleDescriptions(moov)
	if err != nil {
		return nil, err
	}
	descriptions := make([]ALACSampleDescription, 0, len(locations))
	for _, location := range locations {
		descriptions = append(descriptions, location.ALACSampleDescription)
	}
	return descriptions, nil
}

func findMatchingALACLocation(
	source ALACSampleDescription,
	targets []alacSampleDescriptionLocation,
	used []bool,
) int {
	for index, target := range targets {
		if used[index] ||
			target.TrackID != source.TrackID ||
			target.EntryIndex != source.EntryIndex {
			continue
		}
		return index
	}
	for index, target := range targets {
		if used[index] ||
			target.TrackID != source.TrackID {
			continue
		}
		return index
	}
	for index, target := range targets {
		if used[index] || target.EntryIndex != source.EntryIndex {
			continue
		}
		return index
	}
	return -1
}

// RestoreALACSampleDescriptions restores the source ALAC bit depth into the
// finalized file's audio sample entry and ALACSpecificConfig. Only the moov
// box is read and, when needed, written back; media payload bytes are never
// copied, decoded, or re-encoded.
func RestoreALACSampleDescriptions(
	path string,
	source []ALACSampleDescription,
) error {
	if len(source) == 0 {
		return nil
	}

	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return err
	}
	topLevel, _, moov, err := scanTopLevelMP4(file, info.Size())
	if err != nil {
		return err
	}
	var moovOffset int64 = -1
	for _, box := range topLevel {
		if box.typ == "moov" {
			moovOffset = box.start
			break
		}
	}
	if moovOffset < 0 {
		return errors.New("moov box not present")
	}

	targets, err := locateALACSampleDescriptions(moov)
	if err != nil {
		return err
	}
	if len(targets) < len(source) {
		return fmt.Errorf(
			"finalized file has %d ALAC sample descriptions; source has %d",
			len(targets),
			len(source),
		)
	}

	used := make([]bool, len(targets))
	changed := false
	for _, sourceDescription := range source {
		if sourceDescription.BitDepth == 0 ||
			sourceDescription.BitDepth > 32 {
			return fmt.Errorf(
				"invalid source ALAC bit depth %d",
				sourceDescription.BitDepth,
			)
		}
		targetIndex := findMatchingALACLocation(
			sourceDescription,
			targets,
			used,
		)
		if targetIndex < 0 {
			return fmt.Errorf(
				"could not match finalized ALAC track %d entry %d",
				sourceDescription.TrackID,
				sourceDescription.EntryIndex,
			)
		}
		used[targetIndex] = true
		target := targets[targetIndex]
		bitDepth := sourceDescription.BitDepth
		if binary.BigEndian.Uint16(
			moov[target.sampleSizeOffset:target.sampleSizeOffset+2],
		) != uint16(bitDepth) {
			binary.BigEndian.PutUint16(
				moov[target.sampleSizeOffset:target.sampleSizeOffset+2],
				uint16(bitDepth),
			)
			changed = true
		}
		if moov[target.configSampleSizeOffset] != bitDepth {
			moov[target.configSampleSizeOffset] = bitDepth
			changed = true
		}
	}

	if !changed {
		return nil
	}
	_, err = file.WriteAt(moov, moovOffset)
	return err
}
