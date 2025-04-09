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
    <style>
        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
            line-height: 1.6;
            color: #333;
            max-width: 1200px;
            margin: 0 auto;
            padding: 20px;
            background-color: #f5f7fa;
        }
        h1, h2, h3 {
            color: #2c3e50;
        }
        .header {
            background-color: #34495e;
            color: white;
            padding: 20px;
            border-radius: 5px;
            margin-bottom: 20px;
            display: flex;
            justify-content: space-between;
            align-items: center;
        }
        .header h1 {
            margin: 0;
            color: white;
        }
        .controls {
            display: flex;
            align-items: center;
        }
        .timestamp {
            font-size: 0.9em;
            color: #ccc;
            margin-right: 15px;
        }
        .auto-refresh {
            display: flex;
            align-items: center;
            margin-right: 15px;
            color: #ccc;
            font-size: 0.9em;
        }
        .auto-refresh input {
            margin: 0 5px;
        }
        .refresh-status {
            font-size: 0.8em;
            color: #2ecc71;
            margin-left: 5px;
        }
        .summary {
            background-color: white;
            border-radius: 5px;
            padding: 20px;
            margin-bottom: 20px;
            box-shadow: 0 2px 5px rgba(0,0,0,0.1);
        }
        .summary-grid {
            display: grid;
            grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
            gap: 20px;
            margin-top: 15px;
        }
        .metric-card {
            background-color: #ecf0f1;
            padding: 15px;
            border-radius: 5px;
            text-align: center;
        }
        .refresh-info {
            background-color: #f0f7fa;
            padding: 10px;
            border-radius: 5px;
            margin-top: 10px;
            font-size: 0.85em;
            color: #2c3e50;
        }
        .refresh-info span {
            font-weight: bold;
            color: #16a085;
        }
        .metric-value {
            font-size: 2em;
            font-weight: bold;
            color: #16a085;
            margin: 10px 0;
        }
        .metric-label {
            font-size: 0.9em;
            color: #7f8c8d;
        }
        .services {
            background-color: white;
            border-radius: 5px;
            padding: 20px;
            margin-bottom: 20px;
            box-shadow: 0 2px 5px rgba(0,0,0,0.1);
        }
        table {
            width: 100%;
            border-collapse: collapse;
            margin-top: 15px;
        }
        th, td {
            padding: 12px 15px;
            text-align: left;
            border-bottom: 1px solid #ddd;
        }
        th {
            background-color: #f8f9fa;
            font-weight: 600;
        }
        tr:hover {
            background-color: #f5f5f5;
        }
        .pod-table {
            margin-left: 30px;
            margin-top: 5px;
            width: calc(100% - 30px);
        }
        .pod-row td {
            padding-top: 8px;
            padding-bottom: 8px;
            font-size: 0.9em;
        }
        .connection-count {
            font-weight: bold;
            color: #16a085;
        }
        .pod-details {
            color: #7f8c8d;
            font-size: 0.85em;
        }
        .json-link {
            text-align: right;
            margin-top: 20px;
        }
        .json-link a {
            color: #3498db;
            text-decoration: none;
        }
        .json-link a:hover {
            text-decoration: underline;
        }
        .refresh-button {
            background-color: #3498db;
            color: white;
            border: none;
            padding: 8px 15px;
            border-radius: 4px;
            cursor: pointer;
            font-size: 0.9em;
        }
        .refresh-button:hover {
            background-color: #2980b9;
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
                margin-top: 10px;
                flex-wrap: wrap;
            }
            .auto-refresh {
                margin-top: 5px;
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