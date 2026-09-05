from pathlib import Path

p = Path("internal/parser/m3u8.go")
s = p.read_text()

start_marker = "\tvar qualityForFilename string\n\tfor _, variant := range master.Variants {"
end_marker = "\n\tif streamUrl == nil {"

if start_marker not in s:
    raise SystemExit("Expected quality selector block not found; refusing to patch blindly")

start = s.index(start_marker)
end = s.index(end_marker, start)

new = r'''	var qualityForFilename string

	// Select only a stream that actually matches the requested mode.
	// Never fall through to the highest-bandwidth unrelated variant.
	if core.Dl_atmos {
		var bestVariant *m3u8.Variant
		bestBitrate := -1
		for _, variant := range master.Variants {
			if variant.Codecs != "ec-3" || !strings.Contains(variant.Audio, "atmos") {
				continue
			}
			split := strings.Split(variant.Audio, "-")
			if len(split) == 0 {
				continue
			}
			bitrate, err := strconv.Atoi(split[len(split)-1])
			if err != nil || bitrate > *core.Atmos_max {
				continue
			}
			if bitrate > bestBitrate {
				v := variant
				bestVariant = v
				bestBitrate = bitrate
			}
		}
		if bestVariant == nil {
			return "", "", qualityForDisplay, fmt.Errorf("requested Dolby Atmos stream up to %d kbps is not available", *core.Atmos_max)
		}
		streamUrl, _ = masterUrl.Parse(bestVariant.URI)
		qualityForFilename = fmt.Sprintf("%d kbps", bestBitrate)
	} else if core.Dl_aac {
		requested := *core.Aac_type
		var bestVariant *m3u8.Variant
		bestBandwidth := uint64(0)

		for _, variant := range master.Variants {
			isAACLC := variant.Codecs == "mp4a.40.2"
			isHEAAC := variant.Codecs == "mp4a.40.5"
			if !isAACLC && !isHEAAC {
				continue
			}

			audio := strings.ToLower(variant.Audio)
			isBinaural := strings.Contains(audio, "binaural")
			isDownmix := strings.Contains(audio, "downmix")
			isRegular := !isBinaural && !isDownmix

			matches := false
			switch requested {
			case "aac-binaural":
				matches = isBinaural
			case "aac-downmix":
				matches = isDownmix
			case "aac-lc", "aac":
				matches = isAACLC && isRegular
			}
			if !matches {
				continue
			}

			bandwidth := uint64(variant.AverageBandwidth)
			if bestVariant == nil || bandwidth > bestBandwidth {
				v := variant
				bestVariant = v
				bestBandwidth = bandwidth
			}
		}

		if bestVariant == nil {
			return "", "", qualityForDisplay, fmt.Errorf("requested AAC stream type %q is not available", requested)
		}

		streamUrl, _ = masterUrl.Parse(bestVariant.URI)
		switch requested {
		case "aac-binaural":
			qualityForFilename = "AAC Binaural"
		case "aac-downmix":
			qualityForFilename = "AAC Downmix"
		default:
			qualityForFilename = "AAC"
		}
	} else {
		var bestVariant *m3u8.Variant
		bestSampleRate := -1
		bestBitDepth := -1
		bestBandwidth := uint64(0)

		for _, variant := range master.Variants {
			if variant.Codecs != "alac" {
				continue
			}
			split := strings.Split(variant.Audio, "-")
			if len(split) < 2 {
				continue
			}
			sampleRate, err := strconv.Atoi(split[len(split)-2])
			if err != nil || sampleRate > *core.Alac_max {
				continue
			}
			bitDepth, _ := strconv.Atoi(split[len(split)-1])
			bandwidth := uint64(variant.AverageBandwidth)

			if bestVariant == nil ||
				sampleRate > bestSampleRate ||
				(sampleRate == bestSampleRate && bitDepth > bestBitDepth) ||
				(sampleRate == bestSampleRate && bitDepth == bestBitDepth && bandwidth > bestBandwidth) {
				v := variant
				bestVariant = v
				bestSampleRate = sampleRate
				bestBitDepth = bitDepth
				bestBandwidth = bandwidth
			}
		}

		if bestVariant == nil {
			return "", "", qualityForDisplay, fmt.Errorf("requested ALAC stream up to %d Hz is not available", *core.Alac_max)
		}

		streamUrl, _ = masterUrl.Parse(bestVariant.URI)
		qualityForFilename = fmt.Sprintf("%dB-%.1fkHz", bestBitDepth, float64(bestSampleRate)/1000.0)
	}
'''

s = s[:start] + new + s[end:]

old = '''\tif streamUrl == nil {\n\t\tif len(master.Variants) > 0 {\n\t\t\tstreamUrl, _ = masterUrl.Parse(master.Variants[0].URI)\n\t\t} else {\n\t\t\treturn "", "", qualityForDisplay, errors.New("no variants found in playlist")\n\t\t}\n\t}\n'''
replacement = '''\tif streamUrl == nil {\n\t\treturn "", "", qualityForDisplay, errors.New("no matching audio variant found")\n\t}\n'''

if old not in s:
    raise SystemExit("Expected generic stream fallback not found; refusing partial patch")

s = s.replace(old, replacement, 1)
p.write_text(s)
print("Applied exact audio quality selector fix")
