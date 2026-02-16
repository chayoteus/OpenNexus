async function loadServers() {
  const container = document.getElementById('server-list');
  container.textContent = 'Loading...';

  try {
    const res = await fetch('./servers.json', { cache: 'no-store' });
    const data = await res.json();
    const servers = Array.isArray(data.servers) ? data.servers : [];

    if (!servers.length) {
      container.innerHTML = '<p class="muted">No servers listed yet.</p>';
      return;
    }

    const table = document.createElement('table');
    table.className = 'server-table';
    table.innerHTML = `
      <thead>
        <tr>
          <th>Server</th>
          <th>Endpoint</th>
          <th>Website</th>
          <th>Connected</th>
          <th>Messages</th>
        </tr>
      </thead>
      <tbody></tbody>
    `;

    const tbody = table.querySelector('tbody');
    for (const s of servers) {
      const row = document.createElement('tr');
      row.innerHTML = `
        <td>${escapeHtml(s.name || 'Unnamed')}</td>
        <td>${s.url ? `<a href="${escapeAttr(s.url)}" target="_blank" rel="noopener">${escapeHtml(s.url)}</a>` : '<span class="muted">-</span>'}</td>
        <td>${s.website ? `<a href="${escapeAttr(s.website)}" target="_blank" rel="noopener">${escapeHtml(s.website)}</a>` : (s.operator ? escapeHtml(s.operator) : '<span class="muted">-</span>')}</td>
        <td class="stats-connected muted">-</td>
        <td class="stats-messages muted">-</td>
      `;
      tbody.appendChild(row);

      if (s.stats_url) {
        loadStatsForRow(s.stats_url, row);
      }
    }

    container.innerHTML = '';
    container.appendChild(table);
  } catch (e) {
    container.innerHTML = '<p class="muted">Failed to load server list.</p>';
  }
}

function escapeHtml(str) {
  return String(str).replace(/[&<>"']/g, (m) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[m]));
}

function escapeAttr(str) {
  return String(str).replace(/"/g, '&quot;');
}

async function loadStatsForRow(statsUrl, row) {
  try {
    const res = await fetch(statsUrl, { cache: 'no-store' });
    const s = await res.json();
    const connected = row.querySelector('.stats-connected');
    const total = row.querySelector('.stats-messages');
    if (connected) {
      connected.textContent = String(s.connected_agents ?? '-');
      connected.classList.remove('muted');
    }
    if (total) {
      total.textContent = String(s.messages_total ?? '-');
      total.classList.remove('muted');
    }
  } catch (_) {
    const connected = row.querySelector('.stats-connected');
    if (connected) connected.textContent = 'unavailable';
  }
}

loadServers();
