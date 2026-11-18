#!/bin/sh
set -eu
cd "$(dirname "$0")"
ffmpeg -y -framerate 30 -i /tmp/fabo-promo-frames/%04d.jpg -i assets/music.wav -c:v libx264 -preset medium -crf 18 -pix_fmt yuv420p -c:a aac -b:a 192k -t 30 -movflags +faststart fabo-promo-v2-1080p.mp4
