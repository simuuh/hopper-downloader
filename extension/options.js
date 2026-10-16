const DEFAULT_TARGET = 'http://localhost:9667';

const targetInput = document.getElementById('target-url');
const tokenInput = document.getElementById('api-token');
const dlcEnabledInput = document.getElementById('dlc-enabled');
const saveStatus = document.getElementById('save-status');
const form = document.getElementById('settings-form');

async function load() {
  const { targetUrl, dlcInterceptionEnabled } = await chrome.storage.sync.get({
    targetUrl: DEFAULT_TARGET,
    dlcInterceptionEnabled: true,
  });
  const { apiToken } = await chrome.storage.local.get({ apiToken: '' });

  targetInput.value = targetUrl;
  dlcEnabledInput.checked = dlcInterceptionEnabled;
  tokenInput.value = apiToken;
}

function isValidTarget(value) {
  try {
    const u = new URL(value);
    return u.protocol === 'http:' || u.protocol === 'https:';
  } catch {
    return false;
  }
}

form.addEventListener('submit', async (e) => {
  e.preventDefault();
  saveStatus.textContent = '';

  const targetUrl = targetInput.value.trim();
  if (!isValidTarget(targetUrl)) {
    saveStatus.textContent = 'Target URL must be a valid http:// or https:// address.';
    saveStatus.style.color = '#ff6b6b';
    return;
  }

  await chrome.storage.sync.set({
    targetUrl,
    dlcInterceptionEnabled: dlcEnabledInput.checked,
  });
  await chrome.storage.local.set({ apiToken: tokenInput.value.trim() });

  saveStatus.style.color = '#4da3ff';
  saveStatus.textContent = 'Saved.';
  setTimeout(() => {
    saveStatus.textContent = '';
  }, 2000);
});

load();
