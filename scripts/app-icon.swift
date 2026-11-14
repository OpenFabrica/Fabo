// Draws the app icon in every size from the transparent mark: the mark on a
// white rounded-square body filling the macOS icon grid (824pt inside the
// 1024 canvas). A body of that shape keeps macOS 26 from setting the icon on
// its own grey plate, as it does for icons with a transparent ground.
//   swift scripts/app-icon.swift brand/logo/fabo-app-mark.png brand/icon/png
import AppKit

let args = CommandLine.arguments
guard args.count == 3, let mark = NSImage(contentsOfFile: args[1]),
      let markCG = mark.cgImage(forProposedRect: nil, context: nil, hints: nil) else {
  FileHandle.standardError.write("usage: swift scripts/app-icon.swift <mark.png> <out dir>\n".data(using: .utf8)!)
  exit(2)
}

// opaqueBounds is the part of the mark that is drawn, so that the padding
// the mark's file happens to have does not shrink it in the body.
func opaqueBounds(_ image: CGImage) -> CGRect {
  let w = image.width, h = image.height
  var pixels = [UInt8](repeating: 0, count: w * h * 4)
  let ctx = CGContext(data: &pixels, width: w, height: h, bitsPerComponent: 8, bytesPerRow: w * 4,
                      space: CGColorSpaceCreateDeviceRGB(), bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)!
  ctx.draw(image, in: CGRect(x: 0, y: 0, width: w, height: h))
  var minX = w, minY = h, maxX = -1, maxY = -1
  for y in 0..<h {
    for x in 0..<w where pixels[(y * w + x) * 4 + 3] > 24 {
      minX = min(minX, x); maxX = max(maxX, x); minY = min(minY, y); maxY = max(maxY, y)
    }
  }
  // Rows of the bitmap run top-down; CGRect here is in the image's own
  // bottom-up coordinates.
  return CGRect(x: minX, y: h - 1 - maxY, width: maxX - minX + 1, height: maxY - minY + 1)
}

let size = 1024
let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: size, pixelsHigh: size, bitsPerSample: 8, samplesPerPixel: 4,
                           hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)
let ctx = NSGraphicsContext.current!.cgContext
ctx.interpolationQuality = .high

// The body: white, a breath darker at the bottom, with a hairline edge so
// it holds its shape on a light Dock.
let body = CGRect(x: 100, y: 100, width: 824, height: 824)
let shape = CGPath(roundedRect: body, cornerWidth: 185, cornerHeight: 185, transform: nil)
ctx.saveGState()
ctx.addPath(shape); ctx.clip()
let gradient = CGGradient(colorsSpace: CGColorSpaceCreateDeviceRGB(), colors: [
  NSColor(srgbRed: 1, green: 1, blue: 1, alpha: 1).cgColor,
  NSColor(srgbRed: 0.937, green: 0.949, blue: 0.961, alpha: 1).cgColor,
] as CFArray, locations: [0, 1])!
ctx.drawLinearGradient(gradient, start: CGPoint(x: 0, y: body.maxY), end: CGPoint(x: 0, y: body.minY), options: [])
ctx.restoreGState()
ctx.addPath(CGPath(roundedRect: body.insetBy(dx: 1, dy: 1), cornerWidth: 184, cornerHeight: 184, transform: nil))
ctx.setStrokeColor(NSColor(srgbRed: 0, green: 0, blue: 0, alpha: 0.08).cgColor)
ctx.setLineWidth(2)
ctx.strokePath()

// The mark, its drawn part fitted into 70% of the body and centered.
let drawn = opaqueBounds(markCG)
let fit = 824 * 0.70 / max(drawn.width, drawn.height)
let target = CGRect(x: body.midX - drawn.width * fit / 2, y: body.midY - drawn.height * fit / 2,
                    width: drawn.width * fit, height: drawn.height * fit)
ctx.draw(markCG.cropping(to: CGRect(x: drawn.minX, y: CGFloat(markCG.height) - drawn.maxY, width: drawn.width, height: drawn.height))!, in: target)
NSGraphicsContext.current = nil

let master = rep.cgImage!
for px in [1024, 512, 256, 128, 64, 32, 16] {
  let out = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: px, pixelsHigh: px, bitsPerSample: 8, samplesPerPixel: 4,
                             hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
  NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: out)
  NSGraphicsContext.current!.cgContext.interpolationQuality = .high
  NSGraphicsContext.current!.cgContext.draw(master, in: CGRect(x: 0, y: 0, width: px, height: px))
  NSGraphicsContext.current = nil
  let path = "\(args[2])/fabo-app-icon-\(px).png"
  try! out.representation(using: .png, properties: [:])!.write(to: URL(fileURLWithPath: path))
}
