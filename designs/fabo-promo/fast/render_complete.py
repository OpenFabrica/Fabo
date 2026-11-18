from pathlib import Path
import subprocess
import sys
import base as b
from PIL import Image, ImageDraw

ROOT = Path(__file__).parent
# All cut points are exact multiples of the 150 BPM quarter note.
STARTS = (0, 1.6, 4, 6.4, 8.8, 11.2, 13.6, 16, 17.6, 20, 21.6, 23.2, 25.6, 28, 29.6)
LENGTH = 32.0
SPECS = [
    ('containers','Run it. ','Build it.','FABO FOR MACOS',(0,0,2432,1318)),
    ('containers','Your whole ','stack.','CONTAINERS & COMPOSE',(410,82,2410,1163)),
    ('machines','Meet your ','Linux.','LINUX MACHINES · UBUNTU',(395,80,2415,1172)),
    ('linux-shell','Your own ','shell.','PERSISTENT LINUX ENVIRONMENT',(454,96,2364,1128)),
    ('vm','Tune your ','VM.','CPU · MEMORY · LIFECYCLE',(432,60,2422,1135)),
    ('logs','Follow every ','log.','LIVE LOGS · FILTER · SEARCH',(450,210,2400,1264)),
    ('container-shell','Step ','inside.','CONTAINER TERMINAL',(459,239,2389,1282)),
    ('debug','Go deeper. ','Debug.','BUILT-IN DEBUG TOOLBOX',(435,108,2405,1173)),
    ('debug','Inspect. ','Verify.','PROCESS NAMESPACE · LOCAL NETWORK',(465,289,2185,1218)),
    ('files','Explore your ','files.','CONTAINER FILE BROWSER',(454,216,2404,1270)),
    ('scan-progress','Scan your ','images.','IMAGE VULNERABILITY SCAN · DEMO DATA',(432,105,2422,1180)),
    ('scan-results','See the ','findings.','SEVERITY · PACKAGES · FIXED VERSIONS · DEMO DATA',(450,231,2410,1290)),
    ('k8s-pods','Know your ','cluster.','KUBERNETES · POD STATUS · READINESS',(420,100,2410,1175)),
    ('k8s-services','Explore your ','services.','KUBERNETES · SERVICES · PORT MAPPINGS',(420,100,2410,1175)),
]
for n in ROOT.joinpath('demo-assets').glob('*.png'):
    b.shots[n.stem] = Image.open(n).convert('RGB')
# Title font stays consistent with the reference.
def frame(scene, offset):
    im=b.BG[scene%6].copy()
    b.ambient(im,scene%6,offset*1.6)
    if scene==14:
        im=b.compose_frame(5,offset*1.5)
        d=ImageDraw.Draw(im)
        d.rectangle((540,609,1380,694), fill=tuple(im.getpixel((600,650))))
        d.text((960,652),'Containers. Linux. Kubernetes.',font=b.font['sub'],fill='#d4e4da',anchor='mm')
        return im
    name,a,c,label,box=SPECS[scene]
    if scene==10:
        if offset >= 1.05:
            name='scan-progress-end'
        elif offset >= .5:
            name='scan-progress-mid'
    b.headline(im,a,c,label,y=110)
    # Quick camera push, then a short readable hold.
    full=(0,0,2432,1318)
    progress=b.ease(offset/.55)
    start=full if scene in (1,2,4,5,6,7,9,10,12,13) else box
    target=box
    if scene==8:
        start=(435,108,2405,1173)
    box=b.mix_area(start,target,progress)
    scale = b.shots[name].width / 2432
    box = tuple(v * scale for v in box)
    b.window(im,name,box,offset)
    if scene==0:
        d=ImageDraw.Draw(im,'RGBA')
        d.rounded_rectangle((216,194,1704,998),radius=26,fill=(8,18,15,145))
        d.rounded_rectangle((410,605,1510,737),radius=24,fill='#192a24',outline='#80dbc0',width=2)
        cmd='docker --context fabo compose up -d'
        typed=cmd[:int(b.clamp((offset-.1)/.95)*len(cmd))]
        d.text((455,669),'$ '+typed,font=b.font['mono'],fill=b.WHITE,anchor='lm')
    return im

if '--proof' in sys.argv:
    proof=Image.new('RGB',(1600,900))
    for i in range(len(STARTS)):
        f=frame(i,1.15).resize((400,225),b.SMOOTH)
        proof.paste(f,(i%4*400,i//4*225))
    proof.save('/tmp/fabo-complete-proof.jpg',quality=94)
else:
    output=ROOT.parent/'Fabo-Scan-K8s-Demo.mp4'
    command=['ffmpeg','-v','error','-y','-f','rawvideo','-pix_fmt','rgb24','-s','1920x1080','-framerate','30','-i','-',
             '-i',str(ROOT/'score_complete.wav'),'-map','0:v','-map','1:a','-c:v','libx264','-preset','fast','-crf','17',
             '-pix_fmt','yuv420p','-af','volume=-0.8dB','-c:a','aac','-b:a','256k','-movflags','+faststart',str(output)]
    with subprocess.Popen(command,stdin=subprocess.PIPE) as process:
        scene=0
        for k in range(round(LENGTH*30)):
            t=k/30
            if scene+1<len(STARTS) and t>=STARTS[scene+1]:
                scene+=1
            process.stdin.write(frame(scene,t-STARTS[scene]).tobytes())
        process.stdin.close()
        if process.wait():
            raise SystemExit('Encoding failed')
