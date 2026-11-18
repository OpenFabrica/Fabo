"""Generate an original 30-second understated synth bed; no external samples."""
import math, wave, struct, random
random.seed(7)
from pathlib import Path
rate = 48000
chords = [(130.813,164.814,195.998),(110,130.813,164.814),(87.307,110,130.813),(97.999,123.471,146.832)]
out = Path(__file__).parent / 'assets' / 'music.wav'
with wave.open(str(out), 'wb') as w:
    w.setparams((2,2,rate,0,'NONE','not compressed'))
    for i in range(30*rate):
        t=i/rate
        chord=chords[int(t/4)%4]
        phase=t%4
        pad_env=min(phase/0.4,1)*min((4-phase)/0.6,1)
        pad=sum(math.sin(2*math.pi*f*t) for f in chord)*0.035*pad_env
        beat=t%0.5
        note=chord[int(t*2)%3]*4
        pluck=math.sin(2*math.pi*note*beat)*math.exp(-beat*12)*0.05
        pulse=math.sin(2*math.pi*(52*beat+3*(1-math.exp(-beat*35))))*math.exp(-beat*16)*0.15
        hat=(random.random()*2-1)*math.exp(-(t%0.25)*90)*0.035
        snare=(random.random()*2-1)*math.exp(-beat*24)*0.06 if int(t*2)%2 else 0
        pulse += hat+snare
        fade=min(t/0.04,1)*min((30-t)/0.5,1)
        value=int((pad+pluck+pulse)*fade*32767)
        w.writeframesraw(struct.pack('<hh',value,value))
