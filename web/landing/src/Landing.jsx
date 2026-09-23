import { useEffect, useRef, useState } from 'react';

import AeroShards from './AeroShards/AeroShards.jsx';
import AuthForm from './AuthForm.jsx';
import DemoChat from './DemoChat.jsx';
import { vendorLogos } from './logos.js';
import { ArrowRightIcon, ClockIcon, GitPullRequestIcon, MenuIcon, RocketIcon, XIcon } from './icons.jsx';

const MODELS = [
  { vendor: 'anthropic', name: 'Opus 5.5' },
  { vendor: 'openai', name: 'GPT-6 Astra' },
  { vendor: 'openai', name: 'GPT-6 Sol' },
  { vendor: 'anthropic', name: 'Fable 5.1' },
  { vendor: 'spacexai', name: 'Grok 4.7' },
  { vendor: 'google', name: 'Gemini 3.8 Flash' },
  { vendor: 'google', name: 'Gemini 3.1 Pro' },
  { vendor: 'deepseek', name: 'DeepSeek V4.1 Flash' }
];

const REASONS = [
  {
    Icon: ClockIcon,
    title: 'Time savings',
    text: "Don't worry about choosing a model; just keep working calmly. We will handle the selection of the best model for this task."
  },
  {
    Icon: ArrowRightIcon,
    title: 'Migration capability',
    text: 'We will fully transfer all your chats from other services to NeoChat. It’s fast, secure, and free.'
  },
  {
    Icon: RocketIcon,
    title: 'Access to new models from leading AI providers',
    text: 'We have the option to manually select the model; the Manual mode features all the most advanced AI models from the leading providers.'
  },
  {
    Icon: GitPullRequestIcon,
    title: 'Open Source project',
    text: 'NeoChat is fully open source; anyone can view the code, study the structure, create a fork, and so on.'
  }
];

// Fades sections in as they scroll into view inside the landing's own scroller.
function useReveal(scroller) {
  useEffect(() => {
    const root = scroller.current;
    if (!root) return undefined;
    const targets = root.querySelectorAll('[data-reveal]');
    if (!('IntersectionObserver' in window)) {
      targets.forEach(node => node.classList.add('is-revealed'));
      return undefined;
    }
    const observer = new IntersectionObserver(
      entries => {
        entries.forEach(entry => {
          if (!entry.isIntersecting) return;
          entry.target.classList.add('is-revealed');
          observer.unobserve(entry.target);
        });
      },
      { root: root.parentElement, threshold: 0.12, rootMargin: '0px 0px -6% 0px' }
    );
    targets.forEach(node => observer.observe(node));
    return () => observer.disconnect();
  }, [scroller]);
}

function AuthDialog({ auth, onClose }) {
  useEffect(() => {
    const onKey = event => {
      if (event.key === 'Escape') onClose();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [onClose]);

  return (
    <div className="lp-dialog-backdrop" onMouseDown={event => event.target === event.currentTarget && onClose()}>
      <div className="lp-dialog" role="dialog" aria-modal="true" aria-labelledby="lp-dialog-title">
        <button className="lp-dialog-close" type="button" aria-label="Close" onClick={onClose}>
          <XIcon />
        </button>
        <h2 id="lp-dialog-title">Log in to NeoChat</h2>
        <p>Paste your NeoChat API key to start chatting.</p>
        <AuthForm auth={auth} autoFocus />
      </div>
    </div>
  );
}

export default function Landing({ auth }) {
  const pageRef = useRef(null);
  const [authOpen, setAuthOpen] = useState(false);
  const [menuOpen, setMenuOpen] = useState(false);
  useReveal(pageRef);

  const scrollTo = id => {
    setMenuOpen(false);
    document.getElementById(id)?.scrollIntoView({ behavior: 'smooth', block: 'start' });
  };
  const openAuth = () => {
    setMenuOpen(false);
    setAuthOpen(true);
  };

  return (
    <div className="lp" ref={pageRef}>
      <section className="lp-hero" aria-label="NeoChat">
        <div className="lp-hero-bg">
          <AeroShards
            backgroundColor="#120F17"
            shardColor="#ffffff"
            accentColor="#ffffff"
            placement="full"
            flow="stream"
            material="pearl"
            detail="balanced"
            effect="ascii"
            scale={0.9}
            spread={1}
            depth={1.25}
            speed={1}
            spin={1}
            interaction="none"
            density={0.5}
            shardSize={1.1}
            stretch={1}
            turbulence={0.65}
            glow={1}
            edgeSoftness={2}
            bloom={0.5}
            grain={0.0225}
            chromaticAberration={0.0075}
            transitionDuration={1}
            interactionRadius={1.25}
            interactionStrength={0.5}
            rippleIntensity={1}
            holdToGather
            paused={false}
          />
        </div>

        <nav className="lp-nav lp-glass" aria-label="Landing">
          <span className="lp-brand">NeoChat</span>
          <button
            className="lp-nav-menu"
            type="button"
            aria-label={menuOpen ? 'Close menu' : 'Open menu'}
            aria-expanded={menuOpen}
            onClick={() => setMenuOpen(open => !open)}
          >
            {menuOpen ? <XIcon /> : <MenuIcon />}
          </button>
          <div className={`lp-nav-links${menuOpen ? ' is-open' : ''}`}>
            <button type="button" onClick={() => scrollTo('lp-features')}>
              Features
            </button>
            <button type="button" onClick={() => scrollTo('lp-about')}>
              About
            </button>
            <button className="lp-nav-signup" type="button" onClick={openAuth}>
              Sign up
            </button>
          </div>
        </nav>

        <div className="lp-hero-content">
          <div className="lp-tag lp-glass">Say hello to Opus 5.5</div>
          <h1 className="lp-headline">AI workspace that automatically picks the best model for every task</h1>
          <div className="lp-hero-actions">
            <button className="lp-btn-solid" type="button" onClick={openAuth}>
              Start chatting for Free
            </button>
            <button className="lp-btn-glass lp-glass" type="button" onClick={() => scrollTo('lp-demo')}>
              Learn more
            </button>
          </div>
        </div>
      </section>

      <div className="lp-body">
        <section className="lp-demo" id="lp-demo" aria-label="Try NeoChat">
          <div className="lp-device" data-reveal>
            <DemoChat />
            <span className="lp-device-glow" aria-hidden="true" />
          </div>
        </section>

        <section className="lp-models" aria-label="Available models" data-reveal>
          <div className="lp-marquee">
            {[0, 1].map(copy => (
              <ul className="lp-marquee-track" key={copy} aria-hidden={copy === 1 || undefined}>
                {MODELS.map(model => (
                  <li className="lp-model" key={model.name}>
                    <span className="lp-model-logo" dangerouslySetInnerHTML={{ __html: vendorLogos[model.vendor] }} />
                    <span>{model.name}</span>
                  </li>
                ))}
              </ul>
            ))}
          </div>
        </section>

        <section className="lp-about" id="lp-about" data-reveal>
          <h2>Not just an AI aggregator with a router</h2>
          <p>
            NeoChat is a full-fledged AI workspace powered by top-tier flagship models from Anthropic, OpenAI, SpaceX AI,
            and others.
          </p>
        </section>

        <section className="lp-reasons" id="lp-features" aria-labelledby="lp-reasons-title">
          <h3 id="lp-reasons-title" data-reveal>
            4 reasons to switch to NeoChat
          </h3>
          <div className="lp-reason-grid">
            {REASONS.map(({ Icon, title, text }) => (
              <article className="lp-reason" key={title} data-reveal>
                <span className="lp-reason-icon">
                  <Icon />
                </span>
                <h4>{title}</h4>
                <p>{text}</p>
              </article>
            ))}
          </div>
        </section>

        <section className="lp-auth" id="lp-auth" aria-labelledby="lp-auth-title">
          <div className="lp-auth-card" data-reveal>
            <h3 id="lp-auth-title">Log in to NeoChat</h3>
            <p>Paste your NeoChat API key to start chatting.</p>
            <AuthForm auth={auth} />
          </div>
        </section>
      </div>

      {authOpen && <AuthDialog auth={auth} onClose={() => setAuthOpen(false)} />}
    </div>
  );
}
