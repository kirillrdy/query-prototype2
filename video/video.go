// Package video decodes exact sample references and supplies frame functions.
package video

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"binaryquery/format"
)

const MaxFrames = 4096
const GrayWidth, GrayHeight = 160, 90

type Source struct {
	Key, Path string
	Size      int64
	Modified  time.Time
}

func (s Source) Check() error {
	stat, err := os.Stat(s.Path)
	if err != nil {
		return err
	}
	if stat.Size() != s.Size || !stat.ModTime().Equal(s.Modified) {
		return fmt.Errorf("source changed since loading; restart or upload it again")
	}
	return nil
}

type Reference struct {
	ID     string
	Source Source
	Frame  format.Frame
}
type Representation string

const Preview Representation = "preview"
const Gray Representation = "gray"
const Detection Representation = "detection"

func (r Representation) options() (suffix, filter string, args []string, err error) {
	switch r {
	case Preview:
		return ".jpg", ",scale=w='min(1280,iw)':h='min(720,ih)':force_original_aspect_ratio=decrease", []string{"-q:v", "2"}, nil
	case Detection:
		return ".detect.jpg", ",scale=w='min(1280,iw)':h='min(720,ih)':force_original_aspect_ratio=decrease", []string{"-q:v", "2"}, nil
	case Gray:
		return ".gray", ",scale=160:90,format=gray", []string{"-c:v", "rawvideo", "-pix_fmt", "gray", "-f", "image2"}, nil
	}
	return "", "", nil, fmt.Errorf("unknown frame representation %q", r)
}
func (d *Decoder) Path(id string, r Representation) string {
	suffix, _, _, _ := r.options()
	return filepath.Join(d.temp, id+suffix)
}
