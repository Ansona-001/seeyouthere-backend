package content

import "regexp"

// youtubeIDRe and vimeoIDRe match the id formats each provider actually
// issues; vimeoHashRe matches Vimeo's optional unlisted-video hash. These
// are the security boundary for the video block: the frontend never builds
// an iframe src from anything but a provider enum plus an id matching one
// of these.
var (
	youtubeIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
	vimeoIDRe   = regexp.MustCompile(`^[1-9][0-9]{0,11}$`)
	vimeoHashRe = regexp.MustCompile(`^[0-9a-f]{8,16}$`)
)

// validAspects are the frame aspect ratios the video block accepts; "" is
// handled separately by the caller and normalises to "16:9".
var validAspects = map[string]bool{"16:9": true, "4:3": true, "1:1": true, "9:16": true}

// validateVideo normalises and validates a video block's provider-specific
// fields in place, recording issues at path. Kicker, heading and caption are
// ordinary text fields validated by the caller like any other block; the
// poster media reference is likewise added to MediaIDs by the caller.
func validateVideo(iss *issues, path string, b *videoBlockJSON) {
	switch b.Provider {
	case "":
		iss.add(path+".provider", "required", "This field is required.")
	case "youtube":
		if !youtubeIDRe.MatchString(b.VideoID) {
			iss.add(path+".video_id", "invalid_video_id", "Not a valid YouTube or Vimeo video.")
		}
		if b.VimeoHash != "" {
			iss.add(path+".vimeo_hash", "invalid_value", "Not valid for this provider.")
			b.VimeoHash = ""
		}
	case "vimeo":
		if !vimeoIDRe.MatchString(b.VideoID) {
			iss.add(path+".video_id", "invalid_video_id", "Not a valid YouTube or Vimeo video.")
		}
		if b.VimeoHash != "" && !vimeoHashRe.MatchString(b.VimeoHash) {
			iss.add(path+".vimeo_hash", "invalid_value", "Not a valid Vimeo hash.")
			b.VimeoHash = ""
		}
	default:
		iss.add(path+".provider", "invalid_value", "Unknown video provider.")
	}

	if b.Aspect == "" {
		b.Aspect = "16:9"
	} else if !validAspects[b.Aspect] {
		iss.add(path+".aspect", "invalid_value", "Unknown aspect ratio.")
		b.Aspect = "16:9"
	}
}
