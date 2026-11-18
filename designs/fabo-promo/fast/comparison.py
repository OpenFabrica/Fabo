from PIL import ImageDraw, ImageFont
import base as b

BODY = ImageFont.truetype(b.SANS, 38)
HEAD = ImageFont.truetype(b.SANS, 29)
NUM = ImageFont.truetype(b.MONO, 43)
NOTE = ImageFont.truetype(b.SANS, 27)

def four_product_comparison(offset=0):
    """Fully readable from frame zero; sources stay alongside each result."""
    im = b.BG[1].copy()
    b.ambient(im, 1, offset)
    b.headline(im, 'Fabo. ', 'By the numbers.',
               'CONTAINER LIFECYCLE · BRIDGE NETWORK · MEDIAN · MILLISECONDS', y=110)
    d = ImageDraw.Draw(im)
    left, right, top = 170, 1750, 236
    xs = [205, 960, 1260, 1305]
    row_h = 112
    d.rounded_rectangle((left, top, right, top+94+4*row_h), radius=22,
                        fill='#152523', outline='#3c5751', width=2)
    headers = ['Engine', 'Docker Engine API', 'Docker CLI', 'Source']
    for i, value in enumerate(headers):
        d.text((xs[i],top+47),value,font=HEAD,fill='#a8bbb4',anchor='rm' if i in (1,2) else 'lm')
    rows = [
        ('Fabo', '3.43', '15.09', 'Our Mac'),
        ('Kiri', '35.49', '35.94', 'Same Mac'),
        ('Docker Desktop', '213', '243', 'Kiri-published'),
        ('OrbStack', '380', '442', 'Kiri-published'),
    ]
    for j, row in enumerate(rows):
        y = top+94+j*row_h
        if j == 0:
            d.rectangle((left+2,y,right-2,y+row_h-2),fill='#203d34')
        d.line((left+25,y,right-25,y),fill='#35483f',width=1)
        for i, value in enumerate(row):
            d.text((xs[i],y+row_h/2),value,font=NUM if i in (1,2) else BODY if i==0 else HEAD,
                   fill=b.MINT if j==0 else b.WHITE if i<3 else '#b5c6be',
                   anchor='rm' if i in (1,2) else 'lm')
    notes = [
        'Fabo / Kiri: same Mac, 2 vCPU / 2 GiB · API 100 runs / CLI 30 runs',
        'Docker Desktop / OrbStack: Kiri-published, 30 runs · different test environment',
        'Sources: Fabo performance report (14 Nov 2026) · github.com/libracoder/kiri',
    ]
    for j, line in enumerate(notes):
        d.text((960,865+j*45),line,font=NOTE,fill='#b5c6be',anchor='mm')
    return im

def comparison(scene, offset):
    im = b.BG[1 if scene == 14 else 2].copy()
    b.ambient(im, 1, offset)
    local = scene == 14
    b.headline(im, 'Measured on ', 'our Mac.' if local else 'Kiri’s setup.',
               'LOCAL BENCHMARK · API · P50 · MILLISECONDS' if local else 'KIRI-PUBLISHED BENCHMARK · MEDIAN · MILLISECONDS', y=110)
    d = ImageDraw.Draw(im)
    left, right, top = 210,1710,236
    row_h = 112
    if local:
        headers=['Engine / network','Create','Start','Exit → removed','Total']
        rows=[
            ['Kiri · none','0.48','0.42','0.36','1.30'],
            ['Fabo · none','0.28','0.69','1.70','2.76'],
            ['Kiri · bridge','2.09','31.74','1.01','35.49'],
            ['Fabo · bridge','0.31','1.09','1.82','3.43'],
        ]
        xs=[left+35,left+570,left+800,left+1130,right-46]
        best={(0,2),(0,3),(0,4),(1,1),(3,1),(3,2),(2,3),(3,4)}
    else:
        headers=['Engine','Docker Engine API','Docker CLI']
        rows=[['Kiri','2.55','14.2'],['Docker Desktop','213','243'],['OrbStack','380','442']]
        xs=[left+35,left+970,right-70]
        best={(0,1),(0,2)}
        row_h=139
    bottom=top+94+len(rows)*row_h
    d.rounded_rectangle((left,top,right,bottom),radius=22,fill='#152523' if local else '#171f32',outline='#3c5751',width=2)
    for i,h in enumerate(headers):
        d.text((xs[i],top+47),h,font=HEAD,fill='#a8bbb4',anchor='lm' if i==0 else 'rm')
    for j,row in enumerate(rows):
        y=top+94+j*row_h
        if local and j%2:
            d.rectangle((left+2,y,right-2,y+row_h-2),fill='#203d34')
        d.line((left+25,y,right-25,y),fill='#35483f',width=1)
        for i,v in enumerate(row):
            d.text((xs[i],y+row_h/2),v,font=BODY if i==0 else NUM,
                   fill=b.MINT if (j,i) in best else b.WHITE,anchor='lm' if i==0 else 'rm')
    if local:
        notes=[
            'Same Mac · 2 vCPU / 2 GiB · Alpine · 100 runs + 5 warm-ups',
            'Stage medians and total median are calculated independently.',
            'Source · Fabo performance report · 14 Nov 2026',
        ]
    else:
        notes=[
            'Alpine running true, then removed · median of 30 runs',
            'Independent benchmark environment · results as published by Kiri',
            'Source · github.com/libracoder/kiri · README / bench/results.json',
        ]
    for j,line in enumerate(notes):
        d.text((960,865+j*45),line,font=NOTE,fill='#b5c6be',anchor='mm')
    return im
