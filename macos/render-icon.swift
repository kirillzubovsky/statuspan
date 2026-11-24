import AppKit
import Foundation

// Draw site/favicon.svg onto a rounded-rect macOS icon tile. Usage: render-icon input.svg output.png

func fail(_ message: String) -> Never {
    FileHandle.standardError.write(Data((message + "\n").utf8))
    exit(1)
}

let args = CommandLine.arguments
if args.count != 3 {
    fail("usage: render-icon input.svg output.png")
}

let input = URL(fileURLWithPath: args[1])
let output = URL(fileURLWithPath: args[2])
let pixels = 1024

_ = NSApplication.shared

let svgData: Data
do {
    svgData = try Data(contentsOf: input)
} catch {
    fail("could not read \(input.path): \(error)")
}
guard let svg = NSImage(data: svgData), svg.isValid else {
    fail("could not read SVG as an image: \(input.path)")
}

guard let rep = NSBitmapImageRep(
    bitmapDataPlanes: nil,
    pixelsWide: pixels,
    pixelsHigh: pixels,
    bitsPerSample: 8,
    samplesPerPixel: 4,
    hasAlpha: true,
    isPlanar: false,
    colorSpaceName: .deviceRGB,
    bytesPerRow: 0,
    bitsPerPixel: 0
) else {
    fail("could not allocate icon bitmap")
}
rep.size = NSSize(width: pixels, height: pixels)

NSGraphicsContext.saveGraphicsState()
guard let context = NSGraphicsContext(bitmapImageRep: rep) else {
    fail("could not create icon graphics context")
}
context.imageInterpolation = .high
context.shouldAntialias = true
NSGraphicsContext.current = context

let bounds = NSRect(x: 0, y: 0, width: pixels, height: pixels)
NSColor.clear.set()
bounds.fill()

let radius = CGFloat(pixels) * 0.2237
let tile = NSBezierPath(roundedRect: bounds, xRadius: radius, yRadius: radius)
tile.addClip()
NSColor(srgbRed: 34.0 / 255.0, green: 35.0 / 255.0, blue: 41.0 / 255.0, alpha: 1).setFill()
bounds.fill()

let pad = CGFloat(pixels) * 0.18
let inner = bounds.insetBy(dx: pad, dy: pad)
svg.size = inner.size
svg.draw(in: inner, from: NSRect(origin: .zero, size: svg.size), operation: .sourceOver, fraction: 1)

NSGraphicsContext.restoreGraphicsState()

guard let png = rep.representation(using: .png, properties: [:]) else {
    fail("could not encode icon PNG")
}
do {
    try png.write(to: output, options: .atomic)
} catch {
    fail("could not write \(output.path): \(error)")
}
