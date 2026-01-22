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

function sanitizeUrl(value) {
    if (!value) return '';
    try {
        const url = new URL(value);
        if (url.protocol === 'http:' || url.protocol === 'https:') {
            return url.href;
        }
    } catch (error) {
        return '';
    }
    return '';
}

function createStatCard(labelText, valueText, valueColor) {
    const card = document.createElement('div');
    card.className = 'stat-card';

    const label = document.createElement('div');
    label.className = 'stat-label';
    label.textContent = labelText;

    const value = document.createElement('div');
    value.className = 'stat-value';
    value.textContent = valueText;
    if (valueColor) {
        value.style.color = valueColor;
    }

    card.append(label, value);
    return card;
}

function createMetricRow(labelText, valueText) {
    const row = document.createElement('div');
    row.className = 'metric-row';

    const label = document.createElement('span');
    label.className = 'metric-label';
    label.textContent = labelText;

    const value = document.createElement('span');
    value.textContent = valueText;

    row.append(label, value);
    return row;
}

function updateSummary(summary) {
    const container = document.getElementById('endpoints-list');
    const summaryStats = document.getElementById('summary-stats');
    const endpoints = summary && summary.endpoints ? summary.endpoints : [];

    container.replaceChildren();
    summaryStats.replaceChildren();

    if (endpoints.length === 0) {
        const emptyState = document.createElement('div');
        emptyState.className = 'empty-state';
        emptyState.textContent = 'No endpoints configured.';
        container.append(emptyState);
        return;
    }

    const total = endpoints.length;
    const up = endpoints.filter(e => e.up).length;
    const down = total - up;
    const avgLatency = Math.round(endpoints.reduce((acc, e) => acc + e.response_time_ms, 0) / (total || 1));

    summaryStats.append(
        createStatCard('System Status', down > 0 ? 'DEGRADED' : 'OPERATIONAL', down > 0 ? 'var(--error-color)' : 'var(--success-color)')
    );

    const monitorsCard = document.createElement('div');
    monitorsCard.className = 'stat-card';
    const monitorsLabel = document.createElement('div');
    monitorsLabel.className = 'stat-label';
    monitorsLabel.textContent = 'Monitors';
    const monitorsValue = document.createElement('div');
    monitorsValue.className = 'stat-value';
    monitorsValue.textContent = `${total} `;
    const monitorsSpan = document.createElement('span');
    monitorsSpan.style.fontSize = '1rem';
    monitorsSpan.style.color = 'var(--text-secondary)';
    monitorsSpan.textContent = `(${up} UP)`;
    monitorsValue.append(monitorsSpan);
    monitorsCard.append(monitorsLabel, monitorsValue);
    summaryStats.append(monitorsCard);

    summaryStats.append(createStatCard('Global Avg Latency', `${avgLatency}ms`));

    endpoints.forEach(ep => {
        const statusClass = ep.up ? 'status-ok' : 'status-down';
        const statusText = ep.up ? 'UP' : 'DOWN';
        const lastCheck = ep.last_check ? new Date(ep.last_check).toLocaleTimeString() : 'Never';

        const card = document.createElement('div');
        card.className = 'endpoint-card';

        const header = document.createElement('div');
        header.className = 'endpoint-header';

        const name = document.createElement('div');
        name.className = 'endpoint-name';
        const indicator = document.createElement('span');
        indicator.className = `status-indicator ${statusClass}`;
        const nameText = document.createElement('span');
        nameText.textContent = ep.name;
        name.append(indicator, nameText);

        const statusValue = document.createElement('div');
        statusValue.style.fontFamily = 'var(--font-mono)';
        statusValue.style.fontWeight = 'bold';
        statusValue.style.color = ep.up ? 'var(--success-color)' : 'var(--error-color)';
        statusValue.textContent = statusText;

        header.append(name, statusValue);

        const urlLink = document.createElement('a');
        urlLink.className = 'endpoint-url';
        const safeUrl = sanitizeUrl(ep.display_url);
        urlLink.href = safeUrl || '#';
        urlLink.textContent = ep.display_url || '';
        urlLink.target = '_blank';
        urlLink.rel = 'noopener noreferrer';
        if (!safeUrl) {
            urlLink.addEventListener('click', (event) => event.preventDefault());
        }

        const metrics = document.createElement('div');
        metrics.className = 'endpoint-metrics';
        metrics.append(
            createMetricRow('Latency', `${ep.response_time_ms}ms`),
            createMetricRow('Uptime', `${(ep.uptime_percent || 0).toFixed(2)}%`),
            createMetricRow('Last Check', lastCheck)
        );

        card.append(header, urlLink, metrics);
        container.append(card);
    });
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
    incidentContainer.replaceChildren();
    if (data.incidents && data.incidents.length > 0) {
        const sortedIncidents = data.incidents.sort((a, b) => new Date(b.start) - new Date(a.start));

        sortedIncidents.forEach(inc => {
            const start = new Date(inc.start).toLocaleString();
            const end = inc.end ? new Date(inc.end).toLocaleString() : 'Ongoing';
            const isDown = !inc.end;
            const statusClass = isDown ? 'incident-down' : 'incident-issue';
            const statusText = isDown ? 'ACTIVE OUTAGE' : 'RESOLVED';

            const item = document.createElement('div');
            item.className = `timeline-item ${statusClass}`;

            const date = document.createElement('div');
            date.className = 'timeline-date';
            date.textContent = `${start} - ${end}`;

            const title = document.createElement('div');
            title.className = 'timeline-title';
            title.textContent = inc.endpoint;

            const desc = document.createElement('div');
            desc.className = 'timeline-desc';
            const statusStrong = document.createElement('strong');
            statusStrong.textContent = statusText;
            desc.append('Status: ', statusStrong, '. ');
            desc.append(inc.error || 'Service disruption detected.');

            item.append(date, title, desc);
            incidentContainer.append(item);
        });
    } else {
        const emptyState = document.createElement('div');
        emptyState.className = 'empty-state';
        emptyState.textContent = 'NO ACTIVE INCIDENTS RECORDED';
        incidentContainer.append(emptyState);
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
