class SyncPrimitivesApp {
    constructor() {
        this.ws = null;
        this.primitives = {};
        this.goroutines = {};
        this.events = [];
        this.metrics = {};

        this.primitivesCanvas = document.getElementById('primitives-canvas');
        this.primitivesCtx = this.primitivesCanvas.getContext('2d');

        this.goroutinesCanvas = document.getElementById('goroutines-canvas');
        this.goroutinesCtx = this.goroutinesCanvas.getContext('2d');

        this.selectedPrimitive = null;
        this.animationFrame = 0;
        this.lastSequence = 0;

        this.initCanvas();
        this.connect();
        this.initEventListeners();
        this.startAnimation();
    }

    initCanvas() {
        const resize = () => {
            this.primitivesCanvas.width = this.primitivesCanvas.offsetWidth;
            this.primitivesCanvas.height = 500;

            this.goroutinesCanvas.width = this.goroutinesCanvas.offsetWidth;
            this.goroutinesCanvas.height = 300;

            this.render();
        };

        window.addEventListener('resize', resize);
        resize();
    }

    connect() {
        const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
        const wsUrl = `${protocol}//${window.location.host}/ws`;

        this.updateStatus('connecting');
        this.ws = new WebSocket(wsUrl);

        this.ws.onopen = () => {
            console.log('WebSocket connected');
            this.reconnectDelay = 1000; // reset backoff on successful connect
            this.updateStatus('connected');
        };

        this.ws.onclose = () => {
            console.log('WebSocket disconnected');
            this.updateStatus('disconnected');
            this.scheduleReconnect();
        };

        this.ws.onerror = (error) => {
            console.error('WebSocket error:', error);
            this.updateStatus('disconnected');
        };

        this.ws.onmessage = (event) => {
            const message = JSON.parse(event.data);
            this.handleMessage(message);
        };
    }

    scheduleReconnect() {
        if (!this.reconnectDelay) this.reconnectDelay = 1000;
        const delay = this.reconnectDelay;
        this.reconnectDelay = Math.min(this.reconnectDelay * 2, 30000); // cap at 30s
        console.log(`Reconnecting in ${delay}ms...`);
        this.showNotification(`Reconnecting in ${Math.round(delay/1000)}s...`, 'info');
        setTimeout(() => this.connect(), delay);
    }

    handleMessage(message) {
        switch (message.type) {
            case 'initialState':
                this.handleInitialState(message.payload);
                break;
            case 'state':
                this.handleInitialState(message.payload);
                break;
            case 'update':
                this.handleUpdate(message.payload);
                break;
            case 'success':
                console.log('Success:', message.payload.message);
                this.showNotification(message.payload.message, 'success');
                break;
            case 'error':
                // F3: show server errors as visible toast notifications
                console.error('Error:', message.payload.message);
                this.showNotification('Error: ' + message.payload.message, 'error');
                break;
            case 'metrics':
                // Response to a getMetrics request: payload has .global and .primitives
                console.log('Metrics snapshot received:', message.payload);
                this.renderPerPrimitiveMetrics(message.payload);
                break;
            default:
                console.warn('Unknown message type:', message.type);
        }
    }

    handleInitialState(payload) {
        this.primitives = payload.primitives || payload.Primitives || {};
        this.goroutines = payload.goroutines || payload.Goroutines || {};
        this.events = payload.events || payload.Events || [];
        this.metrics = payload.metrics || payload.Metrics || {};
        const sequence = payload.sequence || payload.Sequence;
        if (typeof sequence === 'number') {
            this.lastSequence = sequence;
        }

        this.render();
    }

    handleUpdate(payload) {
        const sequence = payload.sequence || payload.Sequence;
        if (typeof sequence === 'number') {
            if (this.lastSequence !== 0 && sequence > this.lastSequence + 1) {
                this.requestFullRefresh();
            }
            if (sequence > this.lastSequence) {
                this.lastSequence = sequence;
            }
        }

        const changedPrims = payload.primitives || payload.Primitives;
        if (changedPrims) {
            this.primitives = Object.assign({}, this.primitives, changedPrims);
        }

        const deletedPrims = payload.deleted || payload.Deleted;
        if (Array.isArray(deletedPrims)) {
            deletedPrims.forEach((id) => {
                delete this.primitives[id];
                if (this.selectedPrimitive === id) {
                    this.selectedPrimitive = null;
                }
            });
        }

        if (payload.Goroutines || payload.goroutines) {
            this.goroutines = payload.Goroutines || payload.goroutines;
        }
        if (payload.Events || payload.events) {
            this.events = payload.Events || payload.events;
        }
        if (payload.Metrics || payload.metrics) {
            this.metrics = payload.Metrics || payload.metrics;
        }

        this.render();
    }

    requestFullRefresh() {
        this.send('requestFullRefresh', {});
    }

    updateStatus(status) {
        const indicator = document.getElementById('status-indicator');
        const text = document.getElementById('status-text');

        indicator.className = 'status-indicator ' + status;

        const statusText = {
            'connected': 'Connected',
            'disconnected': 'Disconnected',
            'connecting': 'Reconnecting...'
        };

        text.textContent = statusText[status] || 'Unknown';
    }

    initEventListeners() {
        const typeSelect = document.getElementById('primitive-type');
        typeSelect.addEventListener('change', () => this.updatePrimitiveOptions());

        // Stress test button
        const stressBtn = document.getElementById('stress-test-btn');
        if (stressBtn) {
            stressBtn.addEventListener('click', () => this.runStressTest());
        }

        // Get metrics button
        const metricsBtn = document.getElementById('get-metrics-btn');
        if (metricsBtn) {
            metricsBtn.addEventListener('click', () => this.send('getMetrics', {}));
        }

        // Clear all button
        const clearBtn = document.getElementById('clear-all-btn');
        if (clearBtn) {
            clearBtn.addEventListener('click', () => this.clearAll());
        }

        this.updatePrimitiveOptions();
    }

    updatePrimitiveOptions() {
        const type = document.getElementById('primitive-type').value;
        const optionsDiv = document.getElementById('primitive-options');

        optionsDiv.innerHTML = '';

        if (type === 'semaphore') {
            optionsDiv.innerHTML = '<input type="number" id="capacity" placeholder="Capacity" value="10" min="1" />';
        } else if (type === 'barrier') {
            optionsDiv.innerHTML = '<input type="number" id="parties" placeholder="Parties" value="5" min="1" />';
        } else if (type === 'waitgroup') {
            optionsDiv.innerHTML = '<input type="number" id="wg-delta" placeholder="Add delta" value="1" min="1" />';
        } else if (type === 'singleflight') {
            optionsDiv.innerHTML = '<input type="text" id="sf-key" placeholder="Key" value="key1" />';
        }
    }

    send(type, payload) {
        if (this.ws && this.ws.readyState === WebSocket.OPEN) {
            this.ws.send(JSON.stringify({ type, payload }));
        }
    }

    render() {
        this.renderPrimitivesList();
        this.renderGoroutinesList();
        this.renderEventsList();
        this.renderMetrics();
        this.renderPrimitivesCanvas();
        this.renderGoroutinesCanvas();
        this.renderDetailedStats();
    }

    renderPrimitivesList() {
        const list = document.getElementById('primitives-list');
        list.innerHTML = '';

        Object.values(this.primitives).forEach(prim => {
            const item = document.createElement('div');
            item.className = 'primitive-item';
            if (this.selectedPrimitive === prim.ID) {
                item.classList.add('selected');
            }

            // Safe: prim.Type comes from server data but is validated against known set
            const knownTypes = ['RWLock', 'Semaphore', 'Mutex', 'CondVar', 'Barrier',
                                'WaitGroup', 'Once', 'Singleflight'];
            const safeType = knownTypes.includes(prim.Type) ? prim.Type : 'Unknown';
            const typeClass = `type-${safeType.toLowerCase()}`;

            // Build header
            const header = document.createElement('div');
            header.className = 'primitive-header';

            const nameSpan = document.createElement('span');
            nameSpan.className = 'primitive-name';
            nameSpan.textContent = prim.Name; // XSS-safe: textContent
            header.appendChild(nameSpan);

            const typeSpan = document.createElement('span');
            typeSpan.className = 'primitive-type ' + typeClass;
            typeSpan.textContent = safeType; // XSS-safe: textContent
            header.appendChild(typeSpan);

            // Build stats
            const stats = document.createElement('div');
            stats.className = 'primitive-stats';

            const idItem = document.createElement('div');
            idItem.className = 'stat-item';
            const idLabel = document.createElement('span');
            idLabel.textContent = 'ID:';
            const idValue = document.createElement('span');
            idValue.textContent = prim.ID; // XSS-safe: textContent
            idItem.appendChild(idLabel);
            idItem.appendChild(idValue);

            const blockedItem = document.createElement('div');
            blockedItem.className = 'stat-item';
            const blockedLabel = document.createElement('span');
            blockedLabel.textContent = 'Blocked:';
            const blockedValue = document.createElement('span');
            blockedValue.className = 'blocked-count';
            blockedValue.textContent = String(prim.BlockedCount || 0); // numeric, safe
            blockedItem.appendChild(blockedLabel);
            blockedItem.appendChild(blockedValue);

            stats.appendChild(idItem);
            stats.appendChild(blockedItem);

            // Build controls div using delegated listener (no onclick interpolation)
            const controlsDiv = document.createElement('div');
            controlsDiv.className = 'primitive-controls';
            this.renderPrimitiveControls(prim, controlsDiv);

            // Delete button — use dataset, no interpolation into onclick
            const deleteBtn = document.createElement('button');
            deleteBtn.className = 'delete-btn';
            deleteBtn.textContent = 'Delete';
            deleteBtn.dataset.id = prim.ID;
            deleteBtn.addEventListener('click', (e) => {
                e.stopPropagation();
                this.deletePrimitive(prim.ID);
            });

            item.appendChild(header);
            item.appendChild(stats);
            item.appendChild(controlsDiv);
            item.appendChild(deleteBtn);

            item.addEventListener('click', (e) => {
                if (!e.target.classList.contains('delete-btn') && !e.target.closest('.primitive-controls')) {
                    this.selectPrimitive(prim.ID);
                }
            });

            list.appendChild(item);
        });

        if (Object.keys(this.primitives).length === 0) {
            const empty = document.createElement('div');
            empty.className = 'empty-message';
            empty.textContent = 'No primitives created';
            list.appendChild(empty);
        }
    }

    // renderPrimitiveControls builds control buttons into containerEl.
    // Uses data-id / data-op attributes + addEventListener instead of onclick interpolation.
    renderPrimitiveControls(prim, containerEl) {
        const ops = this._opsForType(prim.Type);
        ops.forEach(({label, op}) => {
            const btn = document.createElement('button');
            btn.className = 'control-btn';
            btn.textContent = label;
            btn.dataset.id = prim.ID; // stored in dataset, not interpolated into HTML
            btn.dataset.op = op;
            btn.addEventListener('click', () => {
                this.simulateOperation(prim.ID, op);
            });
            containerEl.appendChild(btn);
        });
    }

    _opsForType(type) {
        switch (type) {
            case 'RWLock':
                return [{label: 'RLock', op: 'rlock'}, {label: 'Lock', op: 'lock'}];
            case 'Semaphore':
                return [{label: 'Acquire', op: 'acquire'}, {label: 'Release', op: 'release'}];
            case 'Mutex':
                return [{label: 'Lock', op: 'lock'}, {label: 'Unlock', op: 'unlock'}];
            case 'CondVar':
                return [{label: 'Signal', op: 'signal'}, {label: 'Broadcast', op: 'broadcast'}];
            case 'Barrier':
                return [{label: 'Wait', op: 'wait'}, {label: 'Reset', op: 'reset'}];
            case 'WaitGroup':
                return [{label: 'Add', op: 'add'}, {label: 'Done', op: 'done'}, {label: 'Wait', op: 'wait'}];
            case 'Once':
                return [{label: 'Do', op: 'do'}, {label: 'Reset', op: 'reset'}];
            case 'Singleflight':
                return [{label: 'Do', op: 'do'}, {label: 'Forget', op: 'forget'}];
            default:
                return [];
        }
    }

    simulateOperation(id, operation) {
        console.log(`Simulating ${operation} on ${id}`);
        this.send('primitiveOp', { id, op: operation });
    }

    renderGoroutinesList() {
        const list = document.getElementById('goroutines-list');
        list.innerHTML = '';

        Object.values(this.goroutines).forEach(g => {
            const item = document.createElement('div');
            // g.State comes from server; validate against known states before use as class
            const knownStates = ['running', 'blocked', 'waiting', 'finished'];
            const safeStateClass = knownStates.includes((g.State || '').toLowerCase())
                ? g.State.toLowerCase() : '';
            item.className = 'goroutine-item' + (safeStateClass ? ' ' + safeStateClass : '');

            const nameDiv = document.createElement('div');
            nameDiv.className = 'goroutine-name';
            nameDiv.textContent = g.Name; // XSS-safe

            const stateDiv = document.createElement('div');
            stateDiv.className = 'goroutine-state';
            // Build state text safely
            let stateText = 'State: ' + (g.State || '');
            if (g.BlockedOn) {
                stateText += ' (on ' + g.BlockedOn + ')';
            }
            stateDiv.textContent = stateText; // XSS-safe

            item.appendChild(nameDiv);
            item.appendChild(stateDiv);
            list.appendChild(item);
        });

        if (Object.keys(this.goroutines).length === 0) {
            const empty = document.createElement('div');
            empty.className = 'empty-message';
            empty.textContent = 'No goroutines';
            list.appendChild(empty);
        }
    }

    renderEventsList() {
        const list = document.getElementById('events-list');
        list.innerHTML = '';

        const recentEvents = this.events.slice(-30).reverse();

        recentEvents.forEach(event => {
            const item = document.createElement('div');
            item.className = 'event-item';

            // event.Timestamp is a value we control (Date parsing), safe numeric result
            const time = new Date(event.Timestamp).toLocaleTimeString();

            const timeDiv = document.createElement('div');
            timeDiv.className = 'event-time';
            timeDiv.textContent = time; // from Date, safe

            const msgDiv = document.createElement('div');
            msgDiv.className = 'event-message';
            msgDiv.textContent = event.Message; // XSS-safe: textContent

            item.appendChild(timeDiv);
            item.appendChild(msgDiv);
            list.appendChild(item);
        });

        if (recentEvents.length === 0) {
            const empty = document.createElement('div');
            empty.className = 'empty-message';
            empty.textContent = 'No events';
            list.appendChild(empty);
        }
    }

    renderMetrics() {
        if (!this.metrics) return;

        document.getElementById('metric-primitives').textContent = this.metrics.TotalPrimitives || 0;
        document.getElementById('metric-goroutines').textContent = this.metrics.TotalGoroutines || 0;
        document.getElementById('metric-active').textContent = this.metrics.ActiveGoroutines || 0;
        document.getElementById('metric-blocked').textContent = this.metrics.BlockedGoroutines || 0;
        document.getElementById('metric-blocks').textContent = this.metrics.TotalBlocks || 0;

        const avgWait = this.metrics.AvgWaitTime || 0;
        const ms = Math.round(avgWait / 1000000);
        document.getElementById('metric-wait').textContent = ms + 'ms';
    }

    renderPrimitivesCanvas() {
        const canvas = this.primitivesCanvas;
        const ctx = this.primitivesCtx;

        ctx.clearRect(0, 0, canvas.width, canvas.height);

        const primitives = Object.values(this.primitives);
        if (primitives.length === 0) {
            ctx.fillStyle = '#999';
            ctx.font = '16px sans-serif';
            ctx.textAlign = 'center';
            ctx.fillText('No primitives to visualize', canvas.width / 2, canvas.height / 2);
            return;
        }

        const boxWidth = 180;
        const boxHeight = 120;
        const padding = 20;
        const cols = Math.floor((canvas.width - padding) / (boxWidth + padding));

        primitives.forEach((prim, index) => {
            const row = Math.floor(index / cols);
            const col = index % cols;

            const x = padding + col * (boxWidth + padding);
            const y = padding + row * (boxHeight + padding);

            this.drawPrimitiveBox(ctx, prim, x, y, boxWidth, boxHeight);
        });
    }

    drawPrimitiveBox(ctx, prim, x, y, width, height) {
        const colors = {
            'RWLock': '#2196f3',
            'Semaphore': '#9c27b0',
            'Mutex': '#4caf50',
            'CondVar': '#ff9800',
            'Barrier': '#e91e63',
            'WaitGroup': '#00bcd4',
            'Once': '#8bc34a',
            'Singleflight': '#ff5722'
        };

        // Validate prim.Type against known set before any use
        const knownTypes = Object.keys(colors);
        const safeType = knownTypes.includes(prim.Type) ? prim.Type : 'Unknown';

        const color = colors[safeType] || '#666';
        const isSelected = this.selectedPrimitive === prim.ID;

        // Animated glow effect
        const pulse = Math.sin(this.animationFrame * 0.05) * 0.1 + 0.9;

        // Draw shadow
        if (isSelected) {
            ctx.shadowBlur = 20;
            ctx.shadowColor = color;
        }

        // Draw box background
        ctx.fillStyle = color;
        ctx.globalAlpha = 0.15 * pulse;
        ctx.fillRect(x, y, width, height);
        ctx.globalAlpha = 1.0;

        // Draw border
        ctx.strokeStyle = color;
        ctx.lineWidth = isSelected ? 4 : 3;
        ctx.strokeRect(x, y, width, height);

        ctx.shadowBlur = 0;

        // Draw type badge
        ctx.fillStyle = color;
        ctx.fillRect(x, y, width, 30);

        // fillText is safe — canvas does not parse HTML
        ctx.fillStyle = '#fff';
        ctx.font = 'bold 14px Arial';
        ctx.textAlign = 'center';
        ctx.fillText(safeType, x + width / 2, y + 20);

        // Draw name — fillText is safe
        ctx.fillStyle = '#333';
        ctx.font = '13px Arial';
        ctx.fillText(prim.Name, x + width / 2, y + 50);

        // Draw stats — substring of ID is safe in fillText
        ctx.font = '11px Arial';
        ctx.textAlign = 'left';
        const shortID = String(prim.ID).substring(0, 12);
        ctx.fillText(`ID: ${shortID}...`, x + 8, y + 75);

        // Draw blocked count with animation
        if (prim.BlockedCount > 0) {
            const badgeSize = 24 + Math.sin(this.animationFrame * 0.1) * 2;
            ctx.fillStyle = '#f44336';
            ctx.beginPath();
            ctx.arc(x + width - 20, y + height - 20, badgeSize / 2, 0, 2 * Math.PI);
            ctx.fill();

            ctx.fillStyle = 'white';
            ctx.font = 'bold 12px sans-serif';
            ctx.textAlign = 'center';
            ctx.fillText(prim.BlockedCount, x + width - 20, y + height - 16);
        }

        // Draw selection indicator
        if (isSelected) {
            ctx.strokeStyle = '#ffd700';
            ctx.lineWidth = 2;
            ctx.setLineDash([5, 5]);
            ctx.strokeRect(x - 3, y - 3, width + 6, height + 6);
            ctx.setLineDash([]);
        }
    }

    renderGoroutinesCanvas() {
        const canvas = this.goroutinesCanvas;
        const ctx = this.goroutinesCtx;

        ctx.clearRect(0, 0, canvas.width, canvas.height);

        const goroutines = Object.values(this.goroutines);
        if (goroutines.length === 0) {
            ctx.fillStyle = '#999';
            ctx.font = '14px sans-serif';
            ctx.textAlign = 'center';
            ctx.fillText('No goroutines', canvas.width / 2, canvas.height / 2);
            return;
        }

        const barHeight = 25;
        const padding = 5;
        const maxBars = Math.floor(canvas.height / (barHeight + padding));

        goroutines.slice(0, maxBars).forEach((g, index) => {
            const y = padding + index * (barHeight + padding);

            // Draw background
            ctx.fillStyle = '#f5f5f5';
            ctx.fillRect(0, y, canvas.width, barHeight);

            // Draw state bar
            const colors = {
                'Running': '#4caf50',
                'Blocked': '#f44336',
                'Waiting': '#ff9800',
                'Finished': '#9e9e9e'
            };

            const barWidth = canvas.width * 0.85;
            const animatedWidth = g.State === 'Running' ?
                barWidth * (0.9 + Math.sin(this.animationFrame * 0.1 + index) * 0.1) : barWidth;

            ctx.fillStyle = colors[g.State] || '#666';
            ctx.fillRect(0, y, animatedWidth, barHeight);

            // fillText is safe for canvas
            ctx.fillStyle = g.State === 'Finished' ? '#666' : 'white';
            ctx.font = 'bold 12px sans-serif';
            ctx.textAlign = 'left';
            ctx.fillText(g.Name, 8, y + 17);

            ctx.fillStyle = '#333';
            ctx.textAlign = 'right';
            ctx.fillText(g.State, canvas.width - 8, y + 17);
        });
    }

    renderDetailedStats() {
        const statsDiv = document.getElementById('detailed-stats');
        if (!statsDiv) return;

        if (!this.selectedPrimitive) {
            statsDiv.innerHTML = '<div class="empty-message">Select a primitive to view detailed statistics</div>';
            return;
        }

        const prim = this.primitives[this.selectedPrimitive];
        if (!prim) return;

        // Build DOM safely — no server data in innerHTML
        statsDiv.innerHTML = '';

        const headerDiv = document.createElement('div');
        headerDiv.className = 'stats-header';
        const h3 = document.createElement('h3');
        const nameText = document.createTextNode(prim.Name + ' (');
        const typeText = document.createTextNode(prim.Type + ')');
        h3.appendChild(nameText);
        h3.appendChild(typeText);
        headerDiv.appendChild(h3);

        const gridDiv = document.createElement('div');
        gridDiv.className = 'stats-grid';

        const makeCard = (label, value) => {
            const card = document.createElement('div');
            card.className = 'stat-card';
            const lbl = document.createElement('div');
            lbl.className = 'stat-label';
            lbl.textContent = label;
            const val = document.createElement('div');
            val.className = 'stat-value';
            val.textContent = value; // always set via textContent
            card.appendChild(lbl);
            card.appendChild(val);
            return card;
        };

        gridDiv.appendChild(makeCard('ID', prim.ID));
        gridDiv.appendChild(makeCard('Blocked Count', String(prim.BlockedCount || 0)));
        gridDiv.appendChild(makeCard('Created', new Date(prim.CreatedAt).toLocaleTimeString()));

        statsDiv.appendChild(headerDiv);
        statsDiv.appendChild(gridDiv);
    }

    renderPerPrimitiveMetrics(payload) {
        const div = document.getElementById('per-primitive-metrics');
        if (!div) return;
        if (!payload || !payload.primitives || Object.keys(payload.primitives).length === 0) {
            div.innerHTML = '<div class="empty-message">No primitive metrics yet</div>';
            return;
        }

        // Build DOM safely — no server data in innerHTML
        div.innerHTML = '';

        for (const [id, m] of Object.entries(payload.primitives)) {
            const header = document.createElement('div');
            header.className = 'stats-header';
            const strong = document.createElement('strong');
            strong.textContent = m.Name + ' (' + m.Type + ')'; // XSS-safe
            const idText = document.createTextNode(' \u2014 ' + id); // XSS-safe
            header.appendChild(strong);
            header.appendChild(idText);
            div.appendChild(header);

            const grid = document.createElement('div');
            grid.className = 'stats-grid';

            const makeCard = (label, value) => {
                const card = document.createElement('div');
                card.className = 'stat-card';
                const lbl = document.createElement('div');
                lbl.className = 'stat-label';
                lbl.textContent = label;
                const val = document.createElement('div');
                val.className = 'stat-value';
                val.textContent = String(value); // numeric, but use textContent for safety
                card.appendChild(lbl);
                card.appendChild(val);
                return card;
            };

            grid.appendChild(makeCard('Acquires', m.Acquires));
            grid.appendChild(makeCard('Releases', m.Releases));
            grid.appendChild(makeCard('Waits', m.Waits));
            grid.appendChild(makeCard('Timeouts', m.Timeouts));
            div.appendChild(grid);
        }
    }

    selectPrimitive(id) {
        this.selectedPrimitive = id;
        this.render();
    }

    startAnimation() {
        const animate = () => {
            this.animationFrame++;
            this.renderPrimitivesCanvas();
            this.renderGoroutinesCanvas();
            requestAnimationFrame(animate);
        };
        animate();
    }

    showNotification(message, type) {
        const notification = document.createElement('div');
        notification.className = `notification notification-${type}`;
        notification.textContent = message;

        document.body.appendChild(notification);

        setTimeout(() => {
            notification.classList.add('show');
        }, 10);

        setTimeout(() => {
            notification.classList.remove('show');
            setTimeout(() => notification.remove(), 300);
        }, 3000);
    }

    runStressTest() {
        const types = ['rwlock', 'semaphore', 'mutex'];
        let count = 0;

        const interval = setInterval(() => {
            if (count >= 10) {
                clearInterval(interval);
                this.showNotification('Stress test complete!', 'success');
                return;
            }

            const type = types[Math.floor(Math.random() * types.length)];
            const name = `stress-${type}-${count}`;
            const id = `${type}-${Date.now()}-${count}`;

            const messageTypes = {
                'rwlock': 'createRWLock',
                'semaphore': 'createSemaphore',
                'mutex': 'createMutex'
            };

            const payload = { id, name };
            if (type === 'semaphore') {
                payload.capacity = Math.floor(Math.random() * 10) + 1;
            }

            this.send(messageTypes[type], payload);
            count++;
        }, 200);
    }

    clearAll() {
        if (!confirm('Delete all primitives?')) return;

        Object.keys(this.primitives).forEach(id => {
            this.send('deletePrimitive', { id });
        });
    }
}

// showInlineError displays an inline validation error near an input element.
function showInlineError(inputId, message) {
    const input = document.getElementById(inputId);
    if (!input) return;
    let errEl = document.getElementById(inputId + '-error');
    if (!errEl) {
        errEl = document.createElement('span');
        errEl.id = inputId + '-error';
        errEl.style.cssText = 'color:red;font-size:12px;display:block;';
        input.parentNode.insertBefore(errEl, input.nextSibling);
    }
    errEl.textContent = message;
}

function clearInlineError(inputId) {
    const errEl = document.getElementById(inputId + '-error');
    if (errEl) errEl.textContent = '';
}

// Global functions
function createPrimitive() {
    const type = document.getElementById('primitive-type').value;
    const name = document.getElementById('primitive-name').value;

    if (!name) {
        alert('Please enter a name');
        return;
    }

    const id = `${type}-${Date.now()}`;

    const messageTypes = {
        'rwlock': 'createRWLock',
        'semaphore': 'createSemaphore',
        'mutex': 'createMutex',
        'condvar': 'createCondVar',
        'barrier': 'createBarrier',
        'waitgroup': 'createWaitGroup',
        'once': 'createOnce',
        'singleflight': 'createSingleflight'
    };

    const payload = { id, name };

    // F2: validate capacity and parties as positive integers before sending
    if (type === 'semaphore') {
        const raw = document.getElementById('capacity').value;
        const capacity = parseInt(raw, 10);
        if (!Number.isInteger(capacity) || capacity <= 0) {
            showInlineError('capacity', 'Capacity must be a positive integer');
            return;
        }
        clearInlineError('capacity');
        payload.capacity = capacity;
    } else if (type === 'barrier') {
        const raw = document.getElementById('parties').value;
        const parties = parseInt(raw, 10);
        if (!Number.isInteger(parties) || parties <= 0) {
            showInlineError('parties', 'Parties must be a positive integer');
            return;
        }
        clearInlineError('parties');
        payload.parties = parties;
    }

    app.send(messageTypes[type], payload);

    // Clear input
    document.getElementById('primitive-name').value = '';
}

// Initialize app
const app = new SyncPrimitivesApp();

// Make deletePrimitive global
app.deletePrimitive = function(id) {
    this.send('deletePrimitive', { id });
};
