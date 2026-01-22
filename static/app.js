document.addEventListener('DOMContentLoaded', () => {
    const themeToggle = document.getElementById('theme-toggle');
    const storedTheme = localStorage.getItem('theme') || 'dark';
    
    document.documentElement.setAttribute('data-theme', storedTheme);
    
    themeToggle.addEventListener('click', () => {
        const currentTheme = document.documentElement.getAttribute('data-theme');
        const newTheme = currentTheme === 'dark' ? 'light' : 'dark';
        
        document.documentElement.setAttribute('data-theme', newTheme);
        localStorage.setItem('theme', newTheme);
        
        if (window.responseChart) {
            updateChartTheme(window.responseChart, newTheme);
        }
    });

    const ctx = document.getElementById('responseChart').getContext('2d');
    Chart.defaults.font.family = "'Fira Code', monospace";
    Chart.defaults.color = '#858595';
    
    window.responseChart = new Chart(ctx, {
        type: 'line',
        data: {
            labels: [],
            datasets: []
        },
        options: {
            responsive: true,
            maintainAspectRatio: false,
            animation: false,
            interaction: {
                mode: 'index',
                intersect: false,
            },
            plugins: {
                legend: {
                    position: 'top',
                    align: 'end',
                    labels: {
                        usePointStyle: true,
                        boxWidth: 8
                    }
                },
                tooltip: {
                    backgroundColor: 'rgba(17, 17, 20, 0.9)',
                    titleColor: '#e0e0e0',
                    bodyColor: '#e0e0e0',
                    borderColor: '#2a2a35',
                    borderWidth: 1,
                    padding: 10,
                    displayColors: true
                }
            },
            scales: {
                x: {
                    grid: {
                        color: 'rgba(42, 42, 53, 0.3)',
                        borderColor: 'rgba(42, 42, 53, 0.3)'
                    },
                    ticks: {
                        maxRotation: 0,
                        autoSkip: true,
                        maxTicksLimit: 8
                    }
                },
                y: {
                    beginAtZero: true,
                    grid: {
                        color: 'rgba(42, 42, 53, 0.3)',
                        borderColor: 'rgba(42, 42, 53, 0.3)'
                    },
                    title: {
                        display: true,
                        text: 'Latency (ms)'
                    }
                }
            }
        }
    });

    fetchData();
    setInterval(fetchData, 5000);
});

async function fetchData() {
    try {
        const [summaryRes, historyRes] = await Promise.all([
            fetch('/api/summary'),
            fetch('/api/history')
        ]);

        if (summaryRes.ok) {
            const summaryData = await summaryRes.json();
            updateSummary(summaryData);
        }

        if (historyRes.ok) {
            const historyData = await historyRes.json();
            updateHistory(historyData);
        }

        document.getElementById('last-updated').textContent = `SYNC: ${new Date().toLocaleTimeString()}`;
        
    } catch (error) {
        console.error('Data sync failed:', error);
        document.getElementById('last-updated').textContent = 'SYNC ERROR';
    }
}

function updateSummary(summary) {
    const container = document.getElementById('endpoints-list');
    const summaryStats = document.getElementById('summary-stats');
    const endpoints = summary && summary.endpoints ? summary.endpoints : [];
    
    if (endpoints.length === 0) {
        container.innerHTML = '<div class="empty-state">No endpoints configured.</div>';
        return;
    }

    const total = endpoints.length;
    const up = endpoints.filter(e => e.up).length;
    const down = total - up;
    const avgLatency = Math.round(endpoints.reduce((acc, e) => acc + e.response_time_ms, 0) / (total || 1));

    summaryStats.innerHTML = `
        <div class="stat-card">
            <div class="stat-label">System Status</div>
            <div class="stat-value" style="color: ${down > 0 ? 'var(--error-color)' : 'var(--success-color)'}">
                ${down > 0 ? 'DEGRADED' : 'OPERATIONAL'}
            </div>
        </div>
        <div class="stat-card">
            <div class="stat-label">Monitors</div>
            <div class="stat-value">${total} <span style="font-size: 1rem; color: var(--text-secondary)">(${up} UP)</span></div>
        </div>
        <div class="stat-card">
            <div class="stat-label">Global Avg Latency</div>
            <div class="stat-value">${avgLatency}ms</div>
        </div>
    `;

    container.innerHTML = endpoints.map(ep => {
        let statusClass = ep.up ? 'status-ok' : 'status-down';
        let statusText = ep.up ? 'UP' : 'DOWN';
        
        const lastCheck = ep.last_check ? new Date(ep.last_check).toLocaleTimeString() : 'Never';
        
        return `
        <div class="endpoint-card">
            <div class="endpoint-header">
                <div class="endpoint-name">
                    <span class="status-indicator ${statusClass}"></span>
                    ${ep.name}
                </div>
                <div style="font-family: var(--font-mono); font-weight: bold; color: ${ep.up ? 'var(--success-color)' : 'var(--error-color)'}">
                    ${statusText}
                </div>
            </div>
            <a href="${ep.display_url || '#'}" target="_blank" class="endpoint-url">${ep.display_url || ''}</a>
            <div class="endpoint-metrics">
                <div class="metric-row">
                    <span class="metric-label">Latency</span>
                    <span>${ep.response_time_ms}ms</span>
                </div>
                <div class="metric-row">
                    <span class="metric-label">Uptime</span>
                    <span>${(ep.uptime_percent || 0).toFixed(2)}%</span>
                </div>
                <div class="metric-row">
                    <span class="metric-label">Last Check</span>
                    <span>${lastCheck}</span>
                </div>
            </div>
        </div>
        `;
    }).join('');
}

function updateHistory(data) {
    if (!data) return;

    if (data.history && window.responseChart) {
        const chart = window.responseChart;
        let historyMap = {};
        
        if (Array.isArray(data.history)) {
             data.history.forEach(h => {
                 if (!historyMap[h.endpoint]) historyMap[h.endpoint] = [];
                 historyMap[h.endpoint].push(h);
             });
        } else {
            historyMap = data.history;
        }

        let maxPoints = 0;
        let timeLabels = [];
        Object.keys(historyMap).forEach(key => {
            if (historyMap[key].length > maxPoints) {
                maxPoints = historyMap[key].length;
                timeLabels = historyMap[key].map(h => {
                    const d = new Date(h.timestamp);
                    return `${d.getHours()}:${d.getMinutes().toString().padStart(2, '0')}`;
                });
            }
        });

        const datasets = Object.keys(historyMap).map((name, index) => {
            const hue = (index * 137.5) % 360;
            const color = `hsl(${hue}, 70%, 50%)`;
            
            return {
                label: name,
                data: historyMap[name].map(h => h.response_time_ms),
                borderColor: color,
                backgroundColor: 'transparent',
                borderWidth: 2,
                pointRadius: 0,
                pointHoverRadius: 4,
                tension: 0.2
            };
        });

        chart.data.labels = timeLabels;
        chart.data.datasets = datasets;
        chart.update('none');
    }

    const incidentContainer = document.getElementById('incident-timeline');
    if (data.incidents && data.incidents.length > 0) {
        const sortedIncidents = data.incidents.sort((a, b) => new Date(b.start) - new Date(a.start));
        
        incidentContainer.innerHTML = sortedIncidents.map(inc => {
            const start = new Date(inc.start).toLocaleString();
            const end = inc.end ? new Date(inc.end).toLocaleString() : 'Ongoing';
            const isDown = !inc.end;
            const statusClass = isDown ? 'incident-down' : 'incident-issue';
            const statusText = isDown ? 'ACTIVE OUTAGE' : 'RESOLVED';
            
            return `
            <div class="timeline-item ${statusClass}">
                <div class="timeline-date">${start} - ${end}</div>
                <div class="timeline-title">${inc.endpoint}</div>
                <div class="timeline-desc">
                    Status: <strong>${statusText}</strong>. 
                    ${inc.error || 'Service disruption detected.'}
                </div>
            </div>
            `;
        }).join('');
    } else {
        incidentContainer.innerHTML = '<div class="empty-state">NO ACTIVE INCIDENTS RECORDED</div>';
    }
}

function updateChartTheme(chart, theme) {
    if (theme === 'dark') {
        Chart.defaults.color = '#858595';
        chart.options.scales.x.grid.color = 'rgba(42, 42, 53, 0.3)';
        chart.options.scales.y.grid.color = 'rgba(42, 42, 53, 0.3)';
    } else {
        Chart.defaults.color = '#5a5a65';
        chart.options.scales.x.grid.color = 'rgba(200, 200, 210, 0.4)';
        chart.options.scales.y.grid.color = 'rgba(200, 200, 210, 0.4)';
    }
    chart.update();
}
