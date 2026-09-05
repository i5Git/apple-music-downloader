package parser

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"main/internal/core"
	"main/utils/structs"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/grafov/m3u8"
	"github.com/olekukonko/tablewriter"
)

// ExtractMvAudio extracts the best audio stream URL from a music video's master m3u8
func ExtractMvAudio(c string) (string, error) {
	MediaUrl, err := url.Parse(c)
	if err != nil {
		return "", err
	}
	resp, err := http.Get(c)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New(resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	audioString := string(body)
	from, listType, err := m3u8.DecodeFrom(strings.NewReader(audioString), true)
	if err != nil || listType != m3u8.MASTER {
		return "", errors.New("m3u8 not of media type")
	}
	audio := from.(*m3u8.MasterPlaylist)

	var audioPriority = []string{"audio-atmos", "audio-ac3", "audio-stereo-256"}
	if *core.Mv_audio_type == "ac3" {
		audioPriority = []string{"audio-ac3", "audio-stereo-256"}
	} else if *core.Mv_audio_type == "aac" {
		audioPriority = []string{"audio-stereo-256"}
	}

	re := regexp.MustCompile(`_gr(\d+)_`)

	type AudioStream struct {
		URL     string
		Rank    int
		GroupID string
	}
	var audioStreams []AudioStream

	for _, variant := range audio.Variants {
		for _, audiov := range variant.Alternatives {
			if audiov.URI != "" {
				for _, priority := range audioPriority {
					if audiov.GroupId == priority {
						matches := re.FindStringSubmatch(audiov.URI)
						if len(matches) == 2 {
							var rank int
							fmt.Sscanf(matches[1], "%d", &rank)
							streamUrl, _ := MediaUrl.Parse(audiov.URI)
							audioStreams = append(audioStreams, AudioStream{
								URL:     streamUrl.String(),
								Rank:    rank,
								GroupID: audiov.GroupId,
							})
						}
					}
				}
			}
		}
	}
	if len(audioStreams) == 0 {
		return "", errors.New("no suitable audio stream found")
	}
	sort.Slice(audioStreams, func(i, j int) bool {
		return audioStreams[i].Rank > audioStreams[j].Rank
	})
	return audioStreams[0].URL, nil
}

// CheckM3u8 retrieves the m3u8 URL from a connected device
func CheckM3u8(b string, f string, account *structs.Account) (string, error) {
	var EnhancedHls string
	if core.Config.GetM3u8FromDevice {
		adamID := b
		conn, err := net.Dial("tcp", account.GetM3u8Port)
		if err != nil {
			return "none", err
		}
		defer conn.Close()

		adamIDBuffer := []byte(adamID)
		lengthBuffer := []byte{byte(len(adamIDBuffer))}

		_, err = conn.Write(lengthBuffer)
		if err != nil {
			return "none", err
		}
		_, err = conn.Write(adamIDBuffer)
		if err != nil {
			return "none", err
		}
		response, err := bufio.NewReader(conn).ReadBytes('\n')
		if err != nil {
			return "none", err
		}
		response = bytes.TrimSpace(response)
		if len(response) > 0 {
			EnhancedHls = string(response)
		}
	}
	return EnhancedHls, nil
}

func formatAvailability(available bool, quality string) string {
	if !available {
		return "Not Available"
	}
	return quality
}

// ExtractMedia extracts the best media stream URL and quality info from a master m3u8
func ExtractMedia(b string, more_mode bool) (string, string, string, error) {
	masterUrl, err := url.Parse(b)
	if err != nil {
		return "", "", "", err
	}
	resp, err := http.Get(b)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", "", errors.New(resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", "", err
	}
	masterString := string(body)
	from, listType, err := m3u8.DecodeFrom(strings.NewReader(masterString), true)
	if err != nil || listType != m3u8.MASTER {
		return "", "", "", errors.New("m3u8 not of master type")
	}
	master := from.(*m3u8.MasterPlaylist)
	var streamUrl *url.URL
	sort.Slice(master.Variants, func(i, j int) bool {
		return master.Variants[i].AverageBandwidth > master.Variants[j].AverageBandwidth
	})

	var hasAAC, hasLossless, hasHiRes, hasAtmos, hasDolbyAudio bool
	var aacQuality, losslessQuality, hiResQuality, atmosQuality, dolbyAudioQuality string

	for _, variant := range master.Variants {
		if variant.Codecs == "mp4a.40.2" { // AAC
			hasAAC = true
			split := strings.Split(variant.Audio, "-")
			if len(split) >= 3 {
				bitrate, _ := strconv.Atoi(split[2])
				currentBitrate := 0
				if aacQuality != "" {
					fmt.Sscanf(aacQuality, "%d kbps", &currentBitrate)
				}
				if bitrate > currentBitrate {
					aacQuality = fmt.Sprintf("%d kbps", bitrate)
				}
			}
		} else if variant.Codecs == "ec-3" && strings.Contains(variant.Audio, "atmos") { // Dolby Atmos
			hasAtmos = true
			split := strings.Split(variant.Audio, "-")
			if len(split) > 0 {
				bitrateStr := split[len(split)-1]
				if len(bitrateStr) == 4 && bitrateStr[0] == '2' {
					bitrateStr = bitrateStr[1:]
				}
				bitrate, _ := strconv.Atoi(bitrateStr)
				currentBitrate := 0
				if atmosQuality != "" {
					fmt.Sscanf(atmosQuality, "%d kbps", &currentBitrate)
				}
				if bitrate > currentBitrate {
					atmosQuality = fmt.Sprintf("%d kbps", bitrate)
				}
			}
		} else if variant.Codecs == "alac" { // ALAC (Lossless or Hi-Res)
			split := strings.Split(variant.Audio, "-")
			if len(split) >= 3 {
				bitDepth := split[len(split)-1]
				sampleRate := split[len(split)-2]
				sampleRateInt, _ := strconv.Atoi(sampleRate)
				if sampleRateInt > 48000 { // Hi-Res
					hasHiRes = true
					hiResQuality = fmt.Sprintf("%sbit/%.1fkHz", bitDepth, float64(sampleRateInt)/1000.0)
				} else { // Standard Lossless
					hasLossless = true
					losslessQuality = fmt.Sprintf("%sbit/%.1fkHz", bitDepth, float64(sampleRateInt)/1000.0)
				}
			}
		} else if variant.Codecs == "ac-3" { // Dolby Audio
			hasDolbyAudio = true
			split := strings.Split(variant.Audio, "-")
			if len(split) > 0 {
				bitrate, _ := strconv.Atoi(split[len(split)-1])
				dolbyAudioQuality = fmt.Sprintf("%d kbps", bitrate)
			}
		}
	}

	var qualityForDisplay string
	if hasHiRes {
		qualityForDisplay = hiResQuality
	} else if hasLossless {
		qualityForDisplay = losslessQuality
	} else if hasAtmos {
		qualityForDisplay = "Dolby Atmos"
	} else if hasDolbyAudio {
		qualityForDisplay = "Dolby Audio"
	} else if hasAAC {
		qualityForDisplay = "AAC"
	}

	if core.Debug_mode && more_mode {
		fmt.Println("\nDebug: All Available Variants:")
		var data [][]string
		for _, variant := range master.Variants {
			data = append(data, []string{variant.Codecs, variant.Audio, fmt.Sprint(variant.Bandwidth)})
		}
		table := tablewriter.NewWriter(os.Stdout)
		table.SetHeader([]string{"Codec", "Audio", "Bandwidth"})
		table.SetRowLine(true)
		table.AppendBulk(data)
		table.Render()

		fmt.Println("Available Audio Formats:")
		fmt.Println("------------------------")
		fmt.Printf("AAC             : %s\n", formatAvailability(hasAAC, aacQuality))
		fmt.Printf("Lossless        : %s\n", formatAvailability(hasLossless, losslessQuality))
		fmt.Printf("Hi-Res Lossless : %s\n", formatAvailability(hasHiRes, hiResQuality))
		fmt.Printf("Dolby Atmos     : %s\n", formatAvailability(hasAtmos, atmosQuality))
		fmt.Printf("Dolby Audio     : %s\n", formatAvailability(hasDolbyAudio, dolbyAudioQuality))
		fmt.Println("------------------------")

		return "", "", "", nil
	}
	var qualityForFilename string

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

	if streamUrl == nil {
		return "", "", qualityForDisplay, errors.New("no matching audio variant found")
	}
	return streamUrl.String(), qualityForFilename, qualityForDisplay, nil
}

// ExtractVideo extracts the best video stream URL from a master m3u8
func ExtractVideo(c string) (string, error) {
	MediaUrl, err := url.Parse(c)
	if err != nil {
		return "", err
	}
	resp, err := http.Get(c)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New(resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	videoString := string(body)

	from, listType, err := m3u8.DecodeFrom(strings.NewReader(videoString), true)
	if err != nil || listType != m3u8.MASTER {
		return "", errors.New("m3u8 not of media type")
	}
	video := from.(*m3u8.MasterPlaylist)

	var streamUrl *url.URL
	sort.Slice(video.Variants, func(i, j int) bool {
		return video.Variants[i].AverageBandwidth > video.Variants[j].AverageBandwidth
	})

	maxHeight := *core.Mv_max
	for _, variant := range video.Variants {
		re := regexp.MustCompile(`_(\d+)x(\d+)`)
		matches := re.FindStringSubmatch(variant.URI)
		if len(matches) == 3 {
			height, _ := strconv.Atoi(matches[2])
			if height <= maxHeight {
				streamUrl, _ = MediaUrl.Parse(variant.URI)
				break
			}
		}
	}

	if streamUrl == nil {
		if len(video.Variants) > 0 {
			streamUrl, _ = MediaUrl.Parse(video.Variants[0].URI)
		} else {
			return "", errors.New("no suitable video stream found")
		}
	}
	return streamUrl.String(), nil
}
