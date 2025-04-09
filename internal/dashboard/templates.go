package dashboard

import (
	"fmt"
)

// getHTMLHeader returns the HTML header and CSS styles
func getHTMLHeader() string {
	return `<!DOCTYPE html>
<html>
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>K8Trust WebSocket Balancer Metrics</title>
    <link rel="stylesheet" href="https://cdnjs.cloudflare.com/ajax/libs/font-awesome/6.0.0/css/all.min.css">
    <style>
        :root {
            --primary-color: #3498db;
            --primary-dark: #2980b9;
            --secondary-color: #2ecc71;
            --secondary-dark: #27ae60;
            --warning-color: #f39c12;
            --danger-color: #e74c3c;
            --text-color: #34495e;
            --text-light: #7f8c8d;
            --bg-color: #ecf0f1;
            --card-bg: #ffffff;
            --header-bg: linear-gradient(135deg, #3498db, #9b59b6);
            --shadow: 0 4px 6px rgba(0, 0, 0, 0.1);
            --transition: all 0.3s ease;
        }

        * {
            box-sizing: border-box;
            transition: var(--transition);
        }

        body {
            font-family: 'Segoe UI', Tahoma, Geneva, Verdana, sans-serif;
            line-height: 1.6;
            color: var(--text-color);
            max-width: 1200px;
            margin: 0 auto;
            padding: 20px;
            background-color: var(--bg-color);
            background-image: linear-gradient(to bottom right, rgba(236, 240, 241, 0.8), rgba(189, 195, 199, 0.4));
            background-attachment: fixed;
        }

        h1, h2, h3 {
            color: var(--text-color);
            margin-top: 0;
        }

        .header {
            background: var(--header-bg);
            color: white;
            padding: 25px;
            border-radius: 12px;
            margin-bottom: 25px;
            display: flex;
            justify-content: space-between;
            align-items: center;
            box-shadow: var(--shadow);
            position: relative;
            overflow: hidden;
        }

        .header::after {
            content: '';
            position: absolute;
            bottom: -10px;
            right: -10px;
            width: 100px;
            height: 100px;
            background: rgba(255, 255, 255, 0.1);
            border-radius: 50%;
        }

        .header h1 {
            margin: 0;
            color: white;
            font-weight: 600;
            text-shadow: 1px 1px 2px rgba(0, 0, 0, 0.2);
            display: flex;
            align-items: center;
            letter-spacing: 0.5px;
        }

        .header h1 i {
            margin-right: 10px;
            font-size: 1.8rem;
        }

        .controls {
            display: flex;
            align-items: center;
            background: rgba(255, 255, 255, 0.2);
            padding: 10px 15px;
            border-radius: 50px;
        }

        .timestamp {
            font-size: 0.9em;
            color: rgba(255, 255, 255, 0.9);
            margin-right: 15px;
            display: flex;
            align-items: center;
        }

        .timestamp i {
            margin-right: 5px;
        }

        .auto-refresh {
            display: flex;
            align-items: center;
            margin-right: 15px;
            color: rgba(255, 255, 255, 0.9);
            font-size: 0.9em;
            position: relative;
        }

        .auto-refresh input[type="checkbox"] {
            appearance: none;
            -webkit-appearance: none;
            width: 40px;
            height: 20px;
            background: rgba(255, 255, 255, 0.3);
            border-radius: 20px;
            position: relative;
            cursor: pointer;
            margin: 0 8px;
            outline: none;
        }

        .auto-refresh input[type="checkbox"]::after {
            content: '';
            position: absolute;
            top: 2px;
            left: 2px;
            width: 16px;
            height: 16px;
            background: white;
            border-radius: 50%;
            transition: var(--transition);
        }

        .auto-refresh input[type="checkbox"]:checked {
            background: var(--secondary-color);
        }

        .auto-refresh input[type="checkbox"]:checked::after {
            left: 22px;
        }

        .auto-refresh select {
            background: rgba(255, 255, 255, 0.2);
            border: none;
            color: white;
            padding: 5px 10px;
            border-radius: 4px;
            margin-left: 10px;
            outline: none;
            cursor: pointer;
        }

        .auto-refresh select option {
            background: var(--primary-color);
            color: white;
        }

        .refresh-status {
            font-size: 0.8em;
            color: rgba(255, 255, 255, 0.9);
            margin-left: 8px;
            min-width: 100px;
            text-align: center;
        }

        .refresh-button {
            background-color: var(--secondary-color);
            color: white;
            border: none;
            padding: 8px 15px;
            border-radius: 50px;
            cursor: pointer;
            font-size: 0.9em;
            display: flex;
            align-items: center;
            font-weight: 500;
            box-shadow: 0 2px 4px rgba(0, 0, 0, 0.1);
        }

        .refresh-button i {
            margin-right: 5px;
        }

        .refresh-button:hover {
            background-color: var(--secondary-dark);
            transform: translateY(-2px);
            box-shadow: 0 4px 8px rgba(0, 0, 0, 0.15);
        }

        .card {
            background-color: var(--card-bg);
            border-radius: 12px;
            padding: 25px;
            margin-bottom: 25px;
            box-shadow: var(--shadow);
            transition: var(--transition);
        }

        .card:hover {
            transform: translateY(-5px);
            box-shadow: 0 8px 15px rgba(0, 0, 0, 0.1);
        }

        .card h2 {
            position: relative;
            padding-bottom: 12px;
            margin-bottom: 20px;
            display: flex;
            align-items: center;
        }

        .card h2::after {
            content: '';
            position: absolute;
            left: 0;
            bottom: 0;
            width: 50px;
            height: 3px;
            background: var(--primary-color);
            border-radius: 3px;
        }

        .card h2 i {
            margin-right: 10px;
            color: var(--primary-color);
        }

        .summary-grid {
            display: grid;
            grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
            gap: 20px;
            margin-top: 20px;
        }

        .metric-card {
            background: linear-gradient(145deg, #ffffff, #f5f5f5);
            padding: 20px;
            border-radius: 10px;
            text-align: center;
            box-shadow: 0 2px 4px rgba(0, 0, 0, 0.05);
            position: relative;
            overflow: hidden;
            transition: transform 0.3s ease;
        }

        .metric-card:hover {
            transform: translateY(-5px);
        }

        .metric-card::before {
            content: '';
            position: absolute;
            top: 0;
            left: 0;
            width: 4px;
            height: 100%;
            background: var(--primary-color);
        }

        .metric-card:nth-child(2)::before {
            background: var(--secondary-color);
        }

        .metric-card:nth-child(3)::before {
            background: var(--warning-color);
        }

        .metric-card:nth-child(4)::before {
            background: var(--danger-color);
        }

        .metric-icon {
            font-size: 1.8rem;
            margin-bottom: 10px;
            color: var(--primary-color);
            background: rgba(52, 152, 219, 0.1);
            width: 50px;
            height: 50px;
            line-height: 50px;
            border-radius: 50%;
            margin: 0 auto 15px;
        }

        .metric-card:nth-child(2) .metric-icon {
            color: var(--secondary-color);
            background: rgba(46, 204, 113, 0.1);
        }

        .metric-card:nth-child(3) .metric-icon {
            color: var(--warning-color);
            background: rgba(243, 156, 18, 0.1);
        }

        .metric-card:nth-child(4) .metric-icon {
            color: var(--danger-color);
            background: rgba(231, 76, 60, 0.1);
        }

        .metric-value {
            font-size: 2.5em;
            font-weight: 700;
            color: var(--text-color);
            margin: 10px 0;
            line-height: 1;
        }

        .metric-label {
            font-size: 0.9em;
            color: var(--text-light);
            text-transform: uppercase;
            letter-spacing: 1px;
            font-weight: 500;
        }

        table {
            width: 100%;
            border-collapse: collapse;
            margin-top: 15px;
            background: white;
            border-radius: 8px;
            overflow: hidden;
            box-shadow: 0 2px 5px rgba(0, 0, 0, 0.05);
        }

        th, td {
            padding: 15px;
            text-align: left;
            border-bottom: 1px solid #f1f1f1;
        }

        th {
            background-color: #f8f9fa;
            font-weight: 600;
            color: var(--text-color);
            position: relative;
        }

        th:first-child {
            border-top-left-radius: 8px;
        }

        th:last-child {
            border-top-right-radius: 8px;
        }

        tr:last-child td {
            border-bottom: none;
        }

        tr:hover {
            background-color: rgba(52, 152, 219, 0.05);
        }

        .pod-table {
            margin-left: 30px;
            margin-top: 10px;
            width: calc(100% - 30px);
            border-radius: 8px;
            box-shadow: none;
            background: #f9f9f9;
        }

        .pod-table th {
            background-color: rgba(52, 152, 219, 0.1);
            color: var(--primary-color);
            font-weight: 500;
            font-size: 0.9em;
        }

        .pod-row td {
            padding: 12px 15px;
            font-size: 0.95em;
        }

        .connection-count {
            font-weight: 600;
            color: var(--primary-color);
            position: relative;
            display: inline-block;
            padding: 4px 10px;
            background: rgba(52, 152, 219, 0.1);
            border-radius: 15px;
            min-width: 40px;
            text-align: center;
        }

        .pod-details {
            color: var(--text-light);
            font-size: 0.85em;
            display: block;
            margin-top: 3px;
        }

        .service-name {
            font-weight: 600;
            color: var(--text-color);
            display: flex;
            align-items: center;
        }

        .service-name i {
            margin-right: 8px;
            color: var(--primary-color);
        }

        .json-link {
            text-align: right;
            margin-top: 20px;
        }

        .json-link a {
            color: var(--primary-color);
            text-decoration: none;
            display: inline-flex;
            align-items: center;
            background: rgba(52, 152, 219, 0.1);
            padding: 8px 15px;
            border-radius: 50px;
            font-size: 0.9em;
            font-weight: 500;
            transition: var(--transition);
        }

        .json-link a i {
            margin-right: 5px;
        }

        .json-link a:hover {
            background: rgba(52, 152, 219, 0.2);
            transform: translateY(-2px);
        }

        .pod-info {
            display: flex;
            align-items: center;
        }

        .pod-info i {
            margin-right: 8px;
            color: var(--warning-color);
        }

        .node-info {
            display: flex;
            align-items: center;
        }

        .node-info i {
            margin-right: 8px;
            color: var(--secondary-color);
        }

        .ip-info {
            display: flex;
            align-items: center;
        }

        .ip-info i {
            margin-right: 8px;
            color: var(--primary-color);
        }

        @media (max-width: 768px) {
            .summary-grid {
                grid-template-columns: 1fr;
            }
            .header {
                flex-direction: column;
                align-items: flex-start;
            }
            .controls {
                margin-top: 15px;
                flex-wrap: wrap;
                width: 100%;
                justify-content: space-between;
            }
            .auto-refresh {
                margin-top: 10px;
                margin-bottom: 10px;
                width: 100%;
                justify-content: space-between;
            }
            .refresh-status {
                margin-top: 5px;
                width: 100%;
                text-align: center;
            }
            .refresh-button {
                margin-top: 10px;
                width: 100%;
                justify-content: center;
            }
            .pod-table {
                margin-left: 0;
                width: 100%;
            }
        }
    </style>
</head>`
}

// getJavaScript returns the JavaScript code for auto-refresh functionality
func getJavaScript(refreshInterval int) string {
	return fmt.Sprintf(`
    <script>
        let refreshTimer;
        let countdownTimer;
        let countdownValue = %d;
        const statusElement = document.getElementById('refresh-status');
        const autoRefreshCheckbox = document.getElementById('auto-refresh');
        const refreshIntervalSelect = document.getElementById('refresh-interval');
        
        // Initialize the refresh status
        updateRefreshStatus();
        
        // Setup event listeners
        autoRefreshCheckbox.addEventListener('change', toggleAutoRefresh);
        refreshIntervalSelect.addEventListener('change', changeRefreshInterval);
        
        // Start auto-refresh if enabled
        if (autoRefreshCheckbox.checked) {
            startAutoRefresh();
        }
        
        // Function to handle manual refresh button
        function manualRefresh() {
            window.location.reload();
        }
        
        // Function to toggle auto-refresh
        function toggleAutoRefresh() {
            if (autoRefreshCheckbox.checked) {
                startAutoRefresh();
            } else {
                stopAutoRefresh();
            }
        }
        
        // Function to change refresh interval
        function changeRefreshInterval() {
            countdownValue = parseInt(refreshIntervalSelect.value);
            
            // Restart auto-refresh if it's enabled
            if (autoRefreshCheckbox.checked) {
                stopAutoRefresh();
                startAutoRefresh();
            } else {
                updateRefreshStatus();
            }
        }
        
        // Function to start auto-refresh
        function startAutoRefresh() {
            countdownValue = parseInt(refreshIntervalSelect.value);
            updateRefreshStatus();
            
            // Clear any existing timers
            stopAutoRefresh();
            
            // Start countdown
            countdownTimer = setInterval(updateCountdown, 1000);
            
            // Set refresh timer
            refreshTimer = setTimeout(function() {
                window.location.href = window.location.pathname + 
                    '?refreshInterval=' + refreshIntervalSelect.value;
            }, countdownValue * 1000);
        }
        
        // Function to stop auto-refresh
        function stopAutoRefresh() {
            clearTimeout(refreshTimer);
            clearInterval(countdownTimer);
            statusElement.textContent = 'Paused';
        }
        
        // Function to update countdown
        function updateCountdown() {
            countdownValue--;
            updateRefreshStatus();
            
            if (countdownValue <= 0) {
                clearInterval(countdownTimer);
            }
        }
        
        // Function to update refresh status text
        function updateRefreshStatus() {
            if (autoRefreshCheckbox.checked) {
                statusElement.textContent = 'Refreshing in ' + countdownValue + 's';
            } else {
                statusElement.textContent = 'Paused';
            }
        }
    </script>`, refreshInterval)
}