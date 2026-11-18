from pathlib import Path
from math import sin, cos, pi
import subprocess
import wave
import numpy as np
from PIL import Image, ImageDraw, ImageFont, ImageFilter

WORK = Path(__file__).parent
ASSETS = Path.cwd() / 'designs/fabo-promo/assets'
W, H, FPS = 1920, 1080, 30
SERIF = '/System/Library/Fonts/Supplemental/Georgia.ttf'
ITALIC = '/System/Library/Fonts/Supplemental/Georgia Italic.ttf'
SANS = '/System/Library/Fonts/SFNS.ttf'
MONO = '/System/Library/Fonts/Menlo.ttc'
WHITE = '#f7f3eb'
MINT = '#90e5cc'
SMOOTH = Image.Resampling.LANCZOS

def clamp(x):
    return max(0, min(1, x))

def ease(x):
    p = clamp(x)
    return p*p*(3-2*p)

def rgb(value):
    return np.array(tuple(bytes.fromhex(value)), dtype=float)

def background(base, glow, cx):
    yy, xx = np.mgrid[0:H:3, 0:W:3]
    lift = np.exp(-2.6 * (((xx-W*cx)/W)**2 + ((yy-H*.69)/H)**2))
    grain = np.random.default_rng(36).uniform(-.7, .7, lift.shape)
    data = rgb(base) + lift[...,None]*(rgb(glow)-rgb(base)) + grain[...,None]
    return Image.fromarray(data.clip(0,255).astype('uint8')).resize((W,H), Image.Resampling.BICUBIC)

PALETTES = [
    ('111310','282921',.51), ('081715','183a33',.5),
    ('0b101c','222d4e',.53), ('18121a','3e2738',.50),
    ('101815','25423b',.50), ('0d1411','20362d',.49),
]
BG = [background(*p) for p in PALETTES]
shots = {name: Image.open(ASSETS / f'{name}.png').convert('RGB') for name in ('fabo-window','logs','terminal')}
mark = Image.open(ASSETS/'mark.png').convert('RGBA')
mark = mark.crop(mark.getbbox())
font = {name: ImageFont.truetype(face, size) for name,face,size in [
    ('title',SERIF,88), ('accent',ITALIC,88), ('brand',SERIF,146),
    ('label',SANS,24), ('sub',SANS,34), ('mono',MONO,44),
    ('tiny',MONO,24), ('terminal',MONO,31),
]}

def spaced(d, phrase, x, y, fill, spacing=5):
    f = font['label']
    width = sum(f.getlength(c) for c in phrase) + (len(phrase)-1)*spacing
    for c in phrase:
        d.text((x-width/2,y),c,font=f,fill=fill,anchor='lt')
        x += f.getlength(c)+spacing

def headline(im, normal, accent, label, y=92):
    d = ImageDraw.Draw(im)
    a,b = font['title'],font['accent']
    x = (W-a.getlength(normal)-b.getlength(accent))/2
    d.text((x,y), normal, font=a, fill=WHITE, anchor='ls')
    d.text((x+a.getlength(normal),y), accent, font=b, fill=MINT, anchor='ls')
    spaced(d,label,W/2,y+30,'#abbab2')

def ambient(im, scene, t):
    d = ImageDraw.Draw(im, 'RGBA')
    for side in (-1,1):
        cx = 960+side*776
        for i in range(7):
            rr = 112+46*i + 10*sin(t*.85)
            cy = 788 if side < 0 else 425
            d.arc((cx-rr,cy-rr,cx+rr,cy+rr), 208 if side<0 else 26, 322 if side<0 else 155,
                  (152,222,192,20 if scene in (1,4) else 13), 1)
        for i in range(36):
            x = cx + side*(i%6-2)*29 + 11*sin(t+i)
            y = 306+i*18+13*sin(t*.72+i*.6)
            r = 1+(i%4==0)
            d.ellipse((x-r,y-r,x+r,y+r),fill=(164,212,200,31+i%6*6))

VW,VH = 1488,804
MASK = Image.new('L',(VW,VH))
ImageDraw.Draw(MASK).rounded_rectangle((0,0,VW-1,VH-1), radius=26, fill=255)
SHADOW = Image.new('RGBA',(VW+170,VH+170))
ImageDraw.Draw(SHADOW).rounded_rectangle((84,87,VW+84,VH+87), radius=40, fill=(0,0,0,161))
SHADOW = SHADOW.filter(ImageFilter.GaussianBlur(31))

def mix_area(start, end, p):
    return tuple(round(a+(b-a)*p) for a,b in zip(start,end))

def window(im, ref, area, t, mode=None, opacity=1):
    src=shots[ref]
    frame=src.resize((VW,VH), SMOOTH, box=area)
    x,y=216,194
    im.paste(SHADOW,(x-85,y-69),SHADOW)
    im.paste(frame,(x,y),MASK)
    d=ImageDraw.Draw(im,'RGBA')
    d.rounded_rectangle((x,y,x+VW-1,y+VH-1),radius=26,outline=(247,255,250,160),width=1)
    def at(sx,sy):
        return (x+round((sx-area[0])*VW/(area[2]-area[0])),
                y+round((sy-area[1])*VH/(area[3]-area[1])))
    if mode=='compose':
        left,top=at(217,184)
        right,bottom=at(870,398)
        q=ease((t-.65)/.65)
        if q>0:
            d.rounded_rectangle((left,top,right,bottom),radius=15,outline=(36,156,128,round(230*q)),width=3)
    elif mode=='logs':
        left,top=at(422,719)
        right,bottom=at(760,853)
        p=ease((t-.7)/.4)
        for row in range(3):
            fade=1-ease((t-.85-row*.68)/.35)
            if fade:
                ya=at(428,727+row*35)[1]
                yb=at(428,754+row*35)[1]
                d.rectangle((left,ya,right,yb),fill=(255,255,255,round(fade*255)))
        if t>1.2:
            a=ease((t-1.2)/.35)
            l, r, card_y = 688, 1232, 644+round((1-a)*16)
            d.rounded_rectangle((l,card_y,r,card_y+178),radius=21,fill=(17,27,39,round(a*249)),outline=(130,161,201,round(a*240)),width=2)
            for row in range(3):
                q=ease((t-1.35-.68*row)/.30)
                stamp=f'stream-tick-{row+1}'
                d.text((l+36,card_y+28+46*row),stamp,font=font['terminal'],fill=(227,240,249,round(q*255)))
        if p:
            left,top=at(1784,643)
            right,bottom=at(1898,702)
            d.rounded_rectangle((left,top,right,bottom),radius=8,outline=(37,146,121,round(230*p)),width=3)
    elif mode=='terminal':
        left,top=at(423,309)
        right,bottom=at(area[2]-25,552)
        d.rectangle((left,top,right,bottom),fill='#ffffff')
        n=int(clamp((t-.60)/.92)*len('pwd'))
        d.text((left+12,top+14),'/ # '+ 'pwd'[:n],font=font['terminal'],fill='#202833')
        if t>1.66:
            d.text((left+12,top+60),'/',font=font['terminal'],fill='#202833')
            d.text((left+12,top+106),'/ #',font=font['terminal'],fill='#202833')
        if int(t*2)%2==0:
            cx=left+16+round(font['terminal'].getlength('/ # '+ ('pwd'[:n] if t<=1.66 else '')))
            cy=top+(126 if t>1.66 else 34)
            d.rectangle((cx,cy,cx+14,cy+24),fill='#148364')
    if opacity<1:
        shade=round((1-opacity)*255)
        d.rounded_rectangle((x,y,x+VW-1,y+VH-1),radius=26,fill=(11,18,18,shade))

COMMAND='docker --context fabo compose up -d'

def command_card(im,t):
    q=ease(t/.4)
    l, r=410,1510
    y=629+round(20*(1-q))
    d=ImageDraw.Draw(im,'RGBA')
    for n in range(23,0,-2):
        d.rounded_rectangle((l-n,y-n,r+n,y+124+n),radius=26+n,outline=(111,222,184,4))
    d.rounded_rectangle((l,y,r,y+124),radius=26,fill='#1d2926',outline='#73b99e',width=2)
    n=int(clamp((t-.5)/2.6)*len(COMMAND))
    msg=COMMAND[:n]
    d.text((l+39,y+60),'$',font=font['mono'],anchor='lm',fill=MINT)
    d.text((l+86,y+60),msg,font=font['mono'],anchor='lm',fill=WHITE)
    if int(t*2.8)%2==0 or n<len(COMMAND):
        x=l+92+font['mono'].getlength(msg)
        d.rectangle((x,y+34,x+2,y+90),fill=MINT)

DUR=38
STARTS=(0,5.15,11.35,17.65,24.25,32.15)
LABELS=[
    ('It starts with ', 'Compose.', 'DOCKER CLI'),
    ('There’s your ', 'stack.', 'COMPOSE PROJECTS'),
    ('See what’s ', 'happening.', 'LIVE CONTAINER LOGS'),
    ('Open a ', 'shell.', 'INTERACTIVE CONTAINER TERMINAL'),
    ('Back to the ', 'big picture.', 'CONTAINERS · IMAGES · NETWORKS · VOLUMES'),
]

def compose_frame(i,t):
    im=BG[i].copy()
    ambient(im,i,t)
    if i<5:
        headline(im,*LABELS[i],y=110)
    if i==0:
        window(im,'fabo-window',(0,0,1280,692),t,opacity=.31)
        command_card(im,t)
    if i==1:
        window(im,'fabo-window',mix_area((0,0,1280,692),(111,41,1186,622),ease((t-.44)/3.1)),t,'compose')
    if i==2:
        window(im,'logs',mix_area((0,143,2360,1418),(407,532,2047,1418),ease(t/4.4)),t,'logs')
    if i==3:
        window(im,'terminal',mix_area((0,0,2360,1275),(205,23,2250,1128),ease(t/3.9)),t,'terminal')
    if i==4:
        window(im,'fabo-window',mix_area((150,28,1230,612),(0,0,1280,692),ease(t/3.85)),t)
    if i==5:
        size=round(154+8*ease(t/1.8))
        icon=mark.resize((size,size),SMOOTH)
        im.paste(icon,((W-size)//2,267),icon)
        d=ImageDraw.Draw(im)
        d.text((W/2,533),'Fabo',font=font['brand'],fill=WHITE,anchor='mm')
        d.text((W/2,652),'Local containers on macOS.',font=font['sub'],fill='#d4e4da',anchor='mm')
        spaced(d,'GITHUB.COM/OPENFABRICA/FABO',W/2,799,'#91cbb7',3)
    return im

def track():
    sr=48000
    timeline=np.arange(DUR*sr)/sr
    mix=np.zeros((len(timeline),2))
    rng=np.random.default_rng(321)
    def voice(start,length,vol,pan,signal):
        at=round(start*sr)
        t=np.arange(min(round(length*sr),len(mix)-at))/sr
        mono=signal(t)*vol
        mix[at:at+len(mono)]+=mono[:,None]*[1-pan,1+pan]
    def note(f,t):
        return (np.sin(2*pi*f*t + .07*np.sin(2*pi*.5*t))+.19*np.sin(4*pi*f*t))*np.minimum(1,t*29)*np.exp(-t*2.2)*(1-np.minimum(1,t/1.9))
    chords=[(45,52,57,60,64,67),(41,48,55,57,60,64),(48,55,60,64,67,71),(43,50,55,59,62,69)]
    for beat in range(64):
        t=beat*.56
        chord=chords[(beat//8)%4]
        midi=chord[(0,2,4,3,5,3,4,2)[beat%8]]
        voice(t,1.65,.105 if beat%4==0 else .080,(-1)**beat*.24,lambda x,n=midi:note(440*2**((n-69)/12),x))
        if beat%4==0:
            voice(t,2.15,.13,0,lambda x,n=chord[0]:np.sin(2*pi*440*2**((n-69)/12)*x)*np.minimum(1,x*10)*np.exp(-x*1.7)*np.maximum(0,1-x/2.16))
        if beat%2:
            voice(t,.1,.024,.19,lambda x:rng.uniform(-1,1,len(x))*np.exp(-x*68)*np.minimum(1,1000*x))
    for i, t in enumerate(STARTS[1:]):
        voice(t,.7,.082,.1,lambda x,i=i:note(440*2**((67+i-69)/12),x))
    for j in range(len(COMMAND)):
        voice(.5+j*(2.6/len(COMMAND)),.030,.031,-.20+j*.01,lambda x:rng.uniform(-1,1,len(x))*np.exp(-x*150))
    fade=np.minimum(1,timeline/.32)*np.minimum(1,(DUR-timeline)/2.1)
    with wave.open(str(WORK/'score.wav'),'wb') as f:
        f.setparams((2,2,sr,0,'NONE',''))
        f.writeframes((mix*fade[:,None]*32767).astype('<i2').tobytes())

if __name__=='__main__':
    import sys
    covers=[2.7,7.7,14.35,20.75,27.65,35.35]
    if '--proof' in sys.argv:
        proof=Image.new('RGB',(1440,810))
        for i,t in enumerate(covers):
            im=compose_frame(i,t-STARTS[i]).resize((480,270),SMOOTH)
            proof.paste(im,(i%3*480,i//3*270))
        proof.crop((0,0,1440,540)).save(WORK/'proof.jpg',quality=92)
    else:
        track()
        output=Path(sys.argv[1])
        cmd=['ffmpeg','-v','warning','-y','-f','rawvideo','-pix_fmt','rgb24','-s',f'{W}x{H}',
             '-framerate',str(FPS),'-i','-','-i',str(WORK/'score.wav'),'-c:v','libx264','-preset','fast',
             '-crf','17','-pix_fmt','yuv420p','-c:a','aac','-b:a','192k','-movflags','+faststart',str(output)]
        with subprocess.Popen(cmd,stdin=subprocess.PIPE) as proc:
            scene=0
            for k in range(DUR*FPS):
                t=k/FPS
                if scene+1<len(STARTS) and t>=STARTS[scene+1]:
                    scene+=1
                offset=t-STARTS[scene]
                im=compose_frame(scene,offset)
                if scene in (1,5) and offset<.30:
                    prev=compose_frame(scene-1,STARTS[scene]-STARTS[scene-1])
                    im=Image.blend(prev,im,ease(offset/.30))
                proc.stdin.write(im.tobytes())
            proc.stdin.close()
            if proc.wait():
                raise SystemExit('FFmpeg failed to encode the release video.')
