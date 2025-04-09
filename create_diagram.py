from PIL import Image, ImageDraw, ImageFont
import os

# Create a new image with white background
width, height = 1200, 800
image = Image.new('RGB', (width, height), color=(255, 255, 255))
draw = ImageDraw.Draw(image)

# Try to load fonts, fallback to default if not available
try:
    title_font = ImageFont.truetype("Arial", 28)
    header_font = ImageFont.truetype("Arial", 22)
    regular_font = ImageFont.truetype("Arial", 18)
    small_font = ImageFont.truetype("Arial", 16)
except IOError:
    # Fallback to default font
    title_font = ImageFont.load_default()
    header_font = ImageFont.load_default()
    regular_font = ImageFont.load_default()
    small_font = ImageFont.load_default()

# Colors
BOX_COLOR = (220, 230, 242)  # Light blue for boxes
CLIENT_COLOR = (255, 230, 230)  # Light red for client
TRAEFIK_COLOR = (230, 255, 230)  # Light green for Traefik
BACKEND_COLOR = (230, 230, 255)  # Light purple for backend
ARROW_COLOR = (100, 100, 100)  # Dark gray for arrows
TEXT_COLOR = (0, 0, 0)  # Black for text
HIGHLIGHT_COLOR = (255, 200, 0)  # Yellow highlight

# Draw title
draw.text((width//2, 30), "Traefik WebSocket Connection Balancer", 
          fill=TEXT_COLOR, font=title_font, anchor="mm")

# Draw client section
client_box = (50, 100, 250, 200)
draw.rectangle(client_box, fill=CLIENT_COLOR, outline=(0, 0, 0))
draw.text((150, 120), "Client", fill=TEXT_COLOR, font=header_font, anchor="mm")
draw.text((150, 150), "WebSocket Client", fill=TEXT_COLOR, font=regular_font, anchor="mm")
draw.text((150, 170), "Browser/App", fill=TEXT_COLOR, font=regular_font, anchor="mm")

# Draw Traefik section
traefik_box = (400, 100, 800, 250)
draw.rectangle(traefik_box, fill=TRAEFIK_COLOR, outline=(0, 0, 0))
draw.text((600, 120), "Traefik Proxy", fill=TEXT_COLOR, font=header_font, anchor="mm")

# Draw balancer middleware
balancer_box = (450, 150, 750, 220)
draw.rectangle(balancer_box, fill=BOX_COLOR, outline=(0, 0, 0))
draw.text((600, 170), "WebSocket Connection Balancer", fill=TEXT_COLOR, font=regular_font, anchor="mm")
draw.text((600, 190), "Middleware Plugin", fill=TEXT_COLOR, font=small_font, anchor="mm")

# Draw backend services
service_width = 200
service_height = 150
service_gap = 50

# Draw 3 services
for i in range(3):
    x = 150 + i * (service_width + service_gap)
    y = 350
    draw.rectangle((x, y, x + service_width, y + service_height), 
                 fill=BACKEND_COLOR, outline=(0, 0, 0))
    draw.text((x + service_width//2, y + 20), f"Backend Service {i+1}", 
             fill=TEXT_COLOR, font=regular_font, anchor="mm")
    
    # Draw pods within services
    pod_count = 2 if i != 1 else 3  # Service 2 has 3 pods, others have 2
    
    for j in range(pod_count):
        pod_x = x + 30 + (j * 70)
        pod_y = y + 60
        draw.rectangle((pod_x, pod_y, pod_x + 60, pod_y + 60), 
                      fill=(255, 255, 255), outline=(0, 0, 0))
        draw.text((pod_x + 30, pod_y + 20), f"Pod", 
                 fill=TEXT_COLOR, font=small_font, anchor="mm")
        draw.text((pod_x + 30, pod_y + 40), f"{j+1}", 
                 fill=TEXT_COLOR, font=small_font, anchor="mm")

# Draw connections and flows

# Client to Traefik
draw.line((250, 150, 400, 150), fill=ARROW_COLOR, width=3)
# Arrow head
draw.polygon([(390, 145), (400, 150), (390, 155)], fill=ARROW_COLOR)
draw.text((325, 135), "WebSocket", fill=TEXT_COLOR, font=small_font, anchor="mm")
draw.text((325, 155), "Connection", fill=TEXT_COLOR, font=small_font, anchor="mm")

# Traefik to Services
for i in range(3):
    x = 250 + i * (service_width + service_gap)
    # Only draw connection to service 2 (the one with lowest connections)
    if i == 1:
        draw.line((600, 220, x, 350), fill=ARROW_COLOR, width=3)
        # Arrow head
        angle = 0.8  # Approximate angle
        arrow_x = x
        arrow_y = 350
        draw.polygon([(arrow_x-10, arrow_y-5), (arrow_x, arrow_y), (arrow_x-5, arrow_y-10)], 
                    fill=ARROW_COLOR)
        
        # Add note about connection selection
        draw.text((500, 280), "Routes to service", fill=TEXT_COLOR, font=small_font, anchor="mm")
        draw.text((500, 300), "with lowest", fill=TEXT_COLOR, font=small_font, anchor="mm")
        draw.text((500, 320), "connection count", fill=TEXT_COLOR, font=small_font, anchor="mm")

# Draw metrics flow
draw.line((750, 185, 950, 185), fill=ARROW_COLOR, width=2)
draw.polygon([(940, 180), (950, 185), (940, 190)], fill=ARROW_COLOR)

metrics_box = (950, 150, 1150, 220)
draw.rectangle(metrics_box, fill=(255, 245, 220), outline=(0, 0, 0))
draw.text((1050, 170), "Metrics Endpoint", fill=TEXT_COLOR, font=small_font, anchor="mm")
draw.text((1050, 190), "/balancer-metrics", fill=TEXT_COLOR, font=small_font, anchor="mm")

# Draw metrics collection from services
for i in range(3):
    x = 250 + i * (service_width + service_gap)
    draw.line((x + service_width//2, 350, x + service_width//2, 550), fill=ARROW_COLOR, width=2)
    draw.polygon([(x + service_width//2 - 5, 540), 
                (x + service_width//2, 550), 
                (x + service_width//2 + 5, 540)], 
                fill=ARROW_COLOR)

metrics_collection_box = (400, 550, 800, 650)
draw.rectangle(metrics_collection_box, fill=(255, 240, 240), outline=(0, 0, 0))
draw.text((600, 570), "Connection Count Collection", fill=TEXT_COLOR, font=regular_font, anchor="mm")
draw.text((600, 595), "1. Direct Service Metrics (/metric)", fill=TEXT_COLOR, font=small_font, anchor="mm")
draw.text((600, 615), "2. Pod Discovery via IP Scanning", fill=TEXT_COLOR, font=small_font, anchor="mm")
draw.text((600, 635), "3. Connection Count Caching", fill=TEXT_COLOR, font=small_font, anchor="mm")

# Draw discovery process
draw.line((800, 600, 1000, 600), fill=ARROW_COLOR, width=2)
draw.polygon([(990, 595), (1000, 600), (990, 605)], fill=ARROW_COLOR)

discovery_box = (1000, 550, 1150, 700)
draw.rectangle(discovery_box, fill=(240, 255, 240), outline=(0, 0, 0))
draw.text((1075, 570), "Pod Discovery", fill=TEXT_COLOR, font=small_font, anchor="mm")
draw.text((1075, 590), "Methods:", fill=TEXT_COLOR, font=small_font, anchor="mm")
draw.text((1075, 610), "1. Endpoints API", fill=TEXT_COLOR, font=small_font, anchor="mm")
draw.text((1075, 630), "2. Direct DNS", fill=TEXT_COLOR, font=small_font, anchor="mm")
draw.text((1075, 650), "3. IP Scanning", fill=TEXT_COLOR, font=small_font, anchor="mm")
draw.text((1075, 670), "(3rd Octet ±5)", fill=TEXT_COLOR, font=small_font, anchor="mm")

# Add an explanation for IP scanning
draw.text((600, 700), "IP Scanning finds pods across different subnets", 
         fill=TEXT_COLOR, font=regular_font, anchor="mm")
draw.text((600, 730), "Example: Discovers 100.68.32.4 and 100.68.36.4", 
         fill=TEXT_COLOR, font=small_font, anchor="mm")
draw.text((600, 750), "by varying the third octet: 100.68.X.Y", 
         fill=TEXT_COLOR, font=small_font, anchor="mm")

# Background box for the entire diagram
draw.rectangle((30, 70, 1170, 770), outline=(100, 100, 100), width=2)

# Save the image
image.save('diagram.png')
print("Diagram created: diagram.png") 