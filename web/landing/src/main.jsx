import { createRoot } from 'react-dom/client';

import Landing from './Landing.jsx';
import './landing.css';

// frontend.html owns the API key; it exposes window.NeoChatAuth before this
// deferred module runs. The landing is mounted only while signed out, so the
// WebGPU background is torn down as soon as someone logs in.
const auth = window.NeoChatAuth;
const host = document.getElementById('landing-root');
let root = null;

function sync(signedIn) {
  if (signedIn) {
    root?.unmount();
    root = null;
    return;
  }
  if (root) return;
  host.scrollTop = 0;
  root = createRoot(host);
  root.render(<Landing auth={auth} />);
}

if (auth && host) {
  sync(auth.isSignedIn());
  auth.onChange(sync);
}
