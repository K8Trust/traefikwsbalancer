#!/bin/bash

# This script attempts to convert the SVG diagram to PNG using several methods
# One of these methods should work on most systems

echo "Converting diagram.svg to diagram.png..."

# Method 1: Try using ImageMagick
if command -v convert &> /dev/null; then
    echo "Attempting conversion with ImageMagick..."
    convert diagram.svg diagram.png
    if [ -f diagram.png ]; then
        echo "Success! Created diagram.png using ImageMagick"
        exit 0
    fi
fi

# Method 2: Try using rsvg-convert
if command -v rsvg-convert &> /dev/null; then
    echo "Attempting conversion with rsvg-convert..."
    rsvg-convert -o diagram.png diagram.svg
    if [ -f diagram.png ]; then
        echo "Success! Created diagram.png using rsvg-convert"
        exit 0
    fi
fi

# Method 3: Try using Chrome/Chromium headless
if command -v chrome &> /dev/null || command -v chromium &> /dev/null || command -v chromium-browser &> /dev/null; then
    CHROME_CMD=""
    if command -v chrome &> /dev/null; then
        CHROME_CMD="chrome"
    elif command -v chromium &> /dev/null; then
        CHROME_CMD="chromium"
    else
        CHROME_CMD="chromium-browser"
    fi
    
    echo "Attempting conversion with $CHROME_CMD headless..."
    $CHROME_CMD --headless --screenshot=diagram.png file://$(pwd)/diagram.svg
    if [ -f diagram.png ]; then
        echo "Success! Created diagram.png using $CHROME_CMD headless"
        exit 0
    fi
fi

# Method 4: Try using Python with svglib
echo "Attempting conversion with Python/svglib..."
cat > convert_svg.py << 'EOF'
try:
    from svglib.svglib import svg2rlg
    from reportlab.graphics import renderPM
    drawing = svg2rlg('diagram.svg')
    renderPM.drawToFile(drawing, 'diagram.png', fmt='PNG')
    print("Success! Created diagram.png using Python/svglib")
except ImportError:
    print("Error: Required Python packages not installed")
    print("Install with: pip install svglib reportlab")
EOF

python3 convert_svg.py 2>/dev/null || python convert_svg.py 2>/dev/null

if [ -f diagram.png ]; then
    echo "Success! Created diagram.png"
    rm convert_svg.py
    exit 0
fi

# If all else fails, provide manual instructions
echo "Unable to automatically convert SVG to PNG."
echo "To manually convert diagram.svg to PNG:"
echo "1. Open diagram.svg in a web browser"
echo "2. Take a screenshot or use browser's save functionality"
echo "3. Save as diagram.png in this directory"
echo ""
echo "Or install one of these tools and run this script again:"
echo "- ImageMagick: https://imagemagick.org"
echo "- librsvg (rsvg-convert): https://wiki.gnome.org/Projects/LibRsvg"
echo "- Python with svglib: pip install svglib reportlab"

exit 1 