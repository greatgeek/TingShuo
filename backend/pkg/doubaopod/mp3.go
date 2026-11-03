package doubaopod

// MP3 时长解析：逐帧遍历累加每帧采样数，得到精确毫秒数。
// 用于装配时间轴（逐轮高亮）——不能按整文件码率估算，因为装配体里混有
// 会话音频（CBR 48kbps）与静音间隙（CBR 64kbps）两种码率。

var (
	brMPEG1L3  = [16]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}
	brMPEG2L3  = [16]int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0}
	srMPEG1    = [3]int{44100, 48000, 32000}
	srMPEG2    = [3]int{22050, 24000, 16000}
	srMPEG25   = [3]int{11025, 12000, 8000}
)

// MP3DurationMS 返回 MP3 数据的播放时长（毫秒）。解析失败返回 0。
func MP3DurationMS(buf []byte) int64 {
	off := 0
	if len(buf) > 10 && buf[0] == 'I' && buf[1] == 'D' && buf[2] == '3' {
		size := int(buf[6])<<21 | int(buf[7])<<14 | int(buf[8])<<7 | int(buf[9]) // synchsafe
		off = size + 10
	}
	var samples int64
	for off+4 <= len(buf) {
		if buf[off] != 0xFF || buf[off+1]&0xE0 != 0xE0 {
			off++
			continue
		}
		b1, b2 := buf[off+1], buf[off+2]
		version := (b1 >> 3) & 0x03 // 3=MPEG1 2=MPEG2 0=MPEG2.5
		layer := (b1 >> 1) & 0x03   // 1=Layer III
		brIdx := (b2 >> 4) & 0x0F
		srIdx := (b2 >> 2) & 0x03
		padding := int((b2 >> 1) & 0x01)
		if layer != 1 || brIdx == 0 || brIdx == 15 || srIdx == 3 {
			off++
			continue
		}
		var br, sr, spf, frameLen int
		switch version {
		case 3:
			br, sr, spf = brMPEG1L3[brIdx]*1000, srMPEG1[srIdx], 1152
			frameLen = 144*br/sr + padding
		case 2:
			br, sr, spf = brMPEG2L3[brIdx]*1000, srMPEG2[srIdx], 576
			frameLen = 72*br/sr + padding
		case 0:
			br, sr, spf = brMPEG2L3[brIdx]*1000, srMPEG25[srIdx], 576
			frameLen = 72*br/sr + padding
		default:
			off++
			continue
		}
		if frameLen <= 0 {
			off++
			continue
		}
		samples += int64(spf)
		off += frameLen
	}
	if samples == 0 {
		return 0
	}
	// 采样率取首帧即可（装配体各段同 24kHz）
	return samples * 1000 / 24000
}
