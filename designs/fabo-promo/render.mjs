// Run with: ego-browser nodejs < /absolute/path/to/render.mjs
const fs=await import('node:fs/promises');
const task=await taskSpace('Fabo video export');const page=task.page('p1');
await fs.mkdir('/tmp/fabo-promo-frames',{recursive:true});
await page.goto('http://localhost:4311/fabo-promo/?capture&v=3');
await page.waitForFunction(()=>!!window.__animStage);
await page.evaluate(()=>Promise.all([...document.images].map(i=>i.decode())));
await page.cdp('Emulation.setDeviceMetricsOverride',{width:1920,height:1080,deviceScaleFactor:1,mobile:false});
await page.evaluate(()=>{window.__animStage.setPlaying(false)});
for(let i=0;i<900;i++){
 await page.evaluate(t=>{window.__animStage.setTime(t)},i/30);
 await page.evaluate(()=>new Promise(r=>requestAnimationFrame(()=>requestAnimationFrame(r))));
 const {data}=await page.cdp('Page.captureScreenshot',{format:'jpeg',quality:94,clip:{x:0,y:0,width:1920,height:1080,scale:1}});
 await fs.writeFile(`/tmp/fabo-promo-frames/${String(i).padStart(4,'0')}.jpg`,Buffer.from(data,'base64'));
 if(i%150===0)console.log(`frame ${i}/900`);
}

console.log('900 frames exported; task space:',task.spaceId);
