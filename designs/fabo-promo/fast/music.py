from pathlib import Path
import wave

import numpy as np

RATE = 48000
LENGTH = 25.6
BEAT = 60 / 150
CUES = (1.6, 6.4, 11.2, 16.0, 23.2)
RNG = np.random.default_rng(1643)
BUS = np.zeros((round(RATE * LENGTH), 2))
TAU = 2 * np.pi

def hz(note):
    return 440 * 2 ** ((note - 69) / 12)

def envelope(t, attack, release, length):
    return np.minimum(1, t / attack) * np.minimum(1, (length - t) / release)

def add(start, length, gain, synth, pan=0):
    first = round(start * RATE)
    count = min(round(length * RATE), len(BUS) - first)
    if count <= 0:
        return
    t = np.arange(count) / RATE
    samples = synth(t) * gain
    if samples.ndim == 1:
        samples = samples[:, None]
    BUS[first:first + count] += samples * np.sqrt(1 + np.array([-pan, pan]))

def kick(t):
    phase = TAU * (46 * t + 66 * .021 * (1 - np.exp(-t / .021)))
    body = np.sin(phase) * np.exp(-t * 13)
    snap = np.sin(TAU * 1880 * t) * np.exp(-t * 240) * .13
    return (body + snap) * envelope(t, .001, .04, .38)

def snare(t):
    noise = RNG.uniform(-1, 1, len(t))
    bright = noise - .7 * np.roll(noise, 1)
    handclaps = sum(np.exp(-((t - x) / .005) ** 2) for x in (.007, .019, .030))
    tail = .57 * np.exp(-t * 15)
    skin = np.sin(TAU * (159 * t + 2 * (1 - np.exp(-t * 37)))) * np.exp(-t * 40)
    return (bright * (tail + .22 * handclaps) + .53 * skin) * envelope(t, .002, .04, .32)

def hat(t, length, decay):
    n = RNG.uniform(-1, 1, len(t))
    noise = n - np.roll(n, 1)
    metal = np.sin(TAU * 7011 * t) * np.sin(TAU * 2787 * t)
    return (noise * .7 + metal * .25) * np.exp(-t * decay) * envelope(t, .001, .013, length)

def bass(t, pitch, length):
    phase = TAU * hz(pitch) * t
    sub = np.sin(phase)
    grit = np.sin(2 * phase) * .38 + np.sin(3 * phase) * .19 + np.sin(5 * phase) * .07
    return np.tanh(1.28 * (sub + grit)) * envelope(t, .018, .055, length) * (.85 + .15 * np.exp(-12 * t))

def pluck(t, pitch, length):
    p = TAU * hz(pitch) * t
    mod = 1.85 * np.exp(-13 * t) * np.sin(2 * p)
    return (np.sin(p + mod) + .15 * np.sin(p * 2.005)) * np.exp(-5.5 * t) * envelope(t, .007, .13, length)

def chord(t, notes, length):
    channels = []
    duck = .48 + .52 * (1 - np.exp(-((t + .02) % BEAT) * 12))
    for side in (-1, 1):
        y = np.zeros(len(t))
        for n in notes:
            p = TAU * hz(n) * t
            y += (np.sin(p * (1 + side * .0036)) + .25 * np.sin(2*p * (1 - side * .005))
                  + .116 * np.sin(3*p + side * .54) + .066 * np.sin(5*p))
        channels.append(y / len(notes))
    shape = envelope(t, .055, .22, length) * duck * (.74 + .26 * np.exp(-t * 2))
    return np.column_stack(channels) * shape[:, None]

def riser(t, length):
    p = t / length
    n = RNG.uniform(-1, 1, len(t))
    jet = (n - np.roll(n, 1)) * .52 + .26 * np.sin(TAU * (178 * t + 130 * t * t))
    return jet * p**2 * envelope(t, .045, .038, length)

def impact(t):
    phase = TAU * (33 * t + 45 * .04 * (1 - np.exp(-t / .04)))
    low = .65 * np.sin(phase) * np.exp(-3.7 * t)
    n = RNG.uniform(-1, 1, len(t))
    splash = .21 * (n - np.roll(n, 1)) * np.exp(-8.7 * t)
    return (low + splash) * envelope(t, .002, .19, 1.29)

# One rhythmic phrase per cut. Move to higher chord voicings as the window opens.
sections = [0, *CUES]
harmonies = [
    (38, (50, 57, 62, 65, 69)),
    (38, (50, 57, 62, 65, 69)),
    (34, (53, 58, 62, 65, 69)),
    (41, (53, 60, 65, 69, 72)),
    (36, (55, 60, 64, 67, 74)),
    (38, (50, 57, 62, 65, 69, 76)),
]
arp_pattern = (1, 3, 2, 4, 1, 2, 3, 4)
for i, start in enumerate(sections):
    stop = sections[i + 1] if i + 1 < len(sections) else 24.4
    root, notes = harmonies[i]
    add(start, stop - start + .2, .17 + i * .018, lambda t, n=notes, span=stop-start+.2: chord(t, n, span))
    for step in range(int((stop - start - .14) / (BEAT / 2)) + 1):
        at = start + step * BEAT / 2
        n = notes[arp_pattern[step % 8]]
        density = .072 if i == 0 else .115 if i < 4 else .134
        add(at, .68, density, lambda t, n=n: pluck(t, n + (12 if step % 8 in (5,7) else 0), .68), (-1) ** step * .33)
        add(at + BEAT * .74, .69, density * .26, lambda t, n=n: pluck(t, n, .69), (-1) ** (step+1) * .67)
        if i < 5 and (step % 2 or i >= 3):
            open_hat = step % 8 == 7
            span = .19 if open_hat else .072
            add(at, span, (.05 if i else .035) * (1 if step % 2 else .63), lambda t, l=span: hat(t,l,17 if l>.1 else 61), -.30 if step % 3 else .35)
        if i and (step % 4 in (0, 2) or i in (3,4) and step % 8 == 7):
            add(at, .38, .45 if step % 4 == 0 else .39, kick)
        if 0 < i < 5 and (step % 4 == 2 or i >= 3 and step % 16 == 15):
            add(at, .32, .26 if step % 4 == 2 else .12, snare, .08)
        if i and (step % 2 or i >= 3 and step % 4 == 0):
            pitch = root + (12 if step % 8 == 7 else 0)
            add(at, .188, .25 if i >= 3 else .20, lambda t, n=pitch: bass(t, n, .188))
    if i:
        add(start, 1.29, .41 if i in (1,5) else .26, impact)
        add(start + .01, .86, .13, lambda t, n=notes[-1]: pluck(t, n, .86), .22)
    if i + 1 < len(sections):
        length = 1.18 if i in (0,4) else .65
        add(stop - length, length, .23 if i in (0,4) else .16, lambda t, l=length: riser(t,l))
        for j in range(4 if i in (0,4) else 2):
            add(stop - (4 if i in (0,4) else 2)*.108 + j*.108, .32, .085+j*.025, snare, -.2 + j*.13)

# Isolated accents on the opening command and a final resolved chord over the logo.
for at in (.55, 1.45, 2.35, 3.25):
    add(at, .38, .28, kick)
add(23.32, 2.28, .22, lambda t: chord(t, harmonies[-1][1], 2.28))
add(23.32, 1.29, .30, impact)
t = np.arange(len(BUS)) / RATE
mix = np.tanh(BUS * 1.12) * envelope(t, .11, 1.14, LENGTH)[:,None] * .94
with wave.open(str(Path(__file__).with_name('score.wav')), 'wb') as wav:
    wav.setparams((2, 2, RATE, 0, 'NONE', ''))
    wav.writeframes((mix * 32767).astype('<i2').tobytes())
