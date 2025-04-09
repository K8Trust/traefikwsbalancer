from PIL import Image, ImageDraw, ImageFont
import os
import sys

# Configuration
font_size = 15  # Adjust this based on your font
width_multiplier = 10  # Width of each character in pixels
height_multiplier = 20  # Height of each line in pixels
padding = 20  # Padding around the image

# Read ASCII diagram
with open('diagram.txt', 'r') as f:
    lines = f.readlines()

# Remove trailing newline
lines = [line.rstrip('\n') for line in lines]

# Calculate image dimensions
width = max(len(line) for line in lines) * width_multiplier + 2 * padding
height = len(lines) * height_multiplier + 2 * padding

# Create image
image = Image.new('RGB', (width, height), color=(255, 255, 255))
draw = ImageDraw.Draw(image)

# Try to load a monospace font
try:
    if sys.platform == 'darwin':  # macOS
        font = ImageFont.truetype('Menlo', font_size)
    elif sys.platform == 'win32':  # Windows
        font = ImageFont.truetype('Consolas', font_size)
    else:  # Linux and others
        font = ImageFont.truetype('DejaVuSansMono', font_size)
except IOError:
    # Fallback to default
    font = ImageFont.load_default()

# Draw text
for i, line in enumerate(lines):
    draw.text(
        (padding, padding + i * height_multiplier),
        line,
        font=font,
        fill=(0, 0, 0)
    )

# Add a title at the bottom
title = "Traefik WebSocket Connection Balancer - Architecture Diagram"
draw.text(
    (width // 2, height - 10),
    title,
    font=font,
    fill=(100, 100, 100),
    anchor="ms"
)

# Save the image
image.save('diagram.png')
print("Diagram saved as diagram.png") 