import { useEffect, useRef, useState } from 'react';

import {
  ArrowUpIcon,
  AtomIcon,
  ChevronDownIcon,
  ClockIcon,
  GraduationCapIcon,
  MicIcon,
  PlusIcon,
  ShuffleIcon
} from './icons.jsx';

// A signed-out replica of the chat screen. It reuses the app's own classes
// from fluid.css so it looks exactly like the real composer, but it never
// talks to the API: every prompt gets the same canned reply.
const THINKING_MS = 1500;
const CHARACTER_MS = 34;
const DEMO_REPLY = 'To use NeoChat, log in :)';

const SUGGESTIONS = [
  { label: 'Schedule task', Icon: ClockIcon },
  { label: 'Help me understand', Icon: GraduationCapIcon },
  { label: 'Brainstorm an idea', Icon: AtomIcon }
];

// Same pools as renderHomeGreeting() in frontend.html, minus the name slot.
function greetingForHour(hour) {
  let pool;
  if (hour >= 9 && hour <= 13) pool = ['What do we do?', 'How can I help?'];
  else if (hour >= 14 && hour <= 16) pool = ['What shall we discuss?', 'Anything left for today?', 'What should we tackle?'];
  else if (hour >= 17 && hour <= 21) pool = ['Clocking for the evening shift', "Let's talk", 'Good evening'];
  else if (hour >= 22 || hour === 0) pool = ['Late chat?', "Let's think about something", 'What shall we talk about?'];
  else if (hour >= 1 && hour <= 6) pool = ['Not tired yet?', 'Late-night ideas?', 'We meet again'];
  else pool = ['Welcome, early bird', 'Another day, another chat!', "What's on the agenda?"];
  return pool[Math.floor(Math.random() * pool.length)];
}

function formatTimestamp(date) {
  const weekdays = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
  const hours = String(date.getHours()).padStart(2, '0');
  const minutes = String(date.getMinutes()).padStart(2, '0');
  return `${date.getDate()} ${weekdays[date.getDay()]} ${hours}:${minutes}`;
}

export default function DemoChat() {
  const [greeting] = useState(() => greetingForHour(new Date().getHours()));
  const [draft, setDraft] = useState('');
  const [messages, setMessages] = useState([]);
  const [busy, setBusy] = useState(false);
  const logRef = useRef(null);
  const textareaRef = useRef(null);
  const timers = useRef([]);

  useEffect(() => () => timers.current.forEach(clearTimeout), []);

  // Like sendMessage() in the app: pin the newest prompt near the top and let
  // the reply grow beneath it.
  const messageCount = messages.length;
  useEffect(() => {
    const log = logRef.current;
    const prompts = log?.querySelectorAll('.chat-message.user');
    const latest = prompts?.[prompts.length - 1];
    if (latest) log.scrollTop = Math.max(0, latest.offsetTop - 12);
  }, [messageCount]);

  const later = (fn, ms) => {
    timers.current.push(setTimeout(fn, ms));
  };

  const updateReply = (id, patch) => {
    setMessages(list => list.map(message => (message.id === id ? { ...message, ...patch } : message)));
  };

  const streamReply = (id, index = 0) => {
    updateReply(id, { text: DEMO_REPLY.slice(0, index) });
    if (index < DEMO_REPLY.length) {
      later(() => streamReply(id, index + 1), CHARACTER_MS);
    } else {
      updateReply(id, { complete: true });
      setBusy(false);
    }
  };

  const send = () => {
    const text = draft.trim();
    if (!text || busy) return;
    const now = Date.now();
    const replyId = `a${now}`;
    setMessages(list => [
      ...list.map(message => ({ ...message, latest: false })),
      { id: `u${now}`, role: 'user', text, at: new Date(now) },
      { id: replyId, role: 'assistant', text: '', pending: true, latest: true }
    ]);
    setDraft('');
    setBusy(true);
    later(() => {
      updateReply(replyId, { pending: false, thoughtSeconds: Math.max(1, Math.round(THINKING_MS / 1000)) });
      streamReply(replyId, 1);
    }, THINKING_MS);
  };

  const hasChat = messages.length > 0;

  return (
    <div className={`main lp-demo-main${hasChat ? ' has-active-chat' : ''}`}>
      <header className="topbar">
        <span className="topbar-title">{hasChat ? messages[0].text.slice(0, 80) : 'New chat'}</span>
      </header>
      <section className={`content${hasChat ? ' has-chat' : ''}`}>
        <div className="chat-log" ref={logRef} aria-live="polite" aria-label="Demo conversation">
          {messages.map(message =>
            message.role === 'user' ? (
              <article className="chat-message user" key={message.id}>
                <div className="chat-copy">
                  <div className="chat-text">
                    <p>{message.text}</p>
                  </div>
                </div>
                <div className="user-message-meta">
                  <time dateTime={message.at.toISOString()}>{formatTimestamp(message.at)}</time>
                </div>
              </article>
            ) : (
              <article
                className={[
                  'chat-message assistant',
                  message.pending && 'is-pending',
                  message.latest && 'is-latest-response',
                  message.complete && 'is-complete'
                ]
                  .filter(Boolean)
                  .join(' ')}
                key={message.id}
              >
                <div className="chat-copy">
                  {message.thoughtSeconds && (
                    <details className="thought-time">
                      <summary>
                        Thought for {message.thoughtSeconds} {message.thoughtSeconds === 1 ? 'second' : 'seconds'}
                      </summary>
                      <div className="thinking-content">
                        Response started after {message.thoughtSeconds}{' '}
                        {message.thoughtSeconds === 1 ? 'second.' : 'seconds.'}
                      </div>
                    </details>
                  )}
                  <div className="chat-text">
                    {message.pending ? (
                      <div className="thinking-state">
                        <span className="thinking-dot" aria-hidden="true" />
                        <span className="thinking-label" data-text="Thinking">
                          Thinking
                        </span>
                      </div>
                    ) : (
                      <p>{message.text}</p>
                    )}
                  </div>
                </div>
              </article>
            )
          )}
        </div>
        <div className="hero-stage">
          <div className="hero default-hero is-visible">
            <h1 className="home-greeting is-ready">{greeting}</h1>
          </div>
        </div>
        <div className="composer-wrap">
          <div className="composer" onClick={() => textareaRef.current?.focus()}>
            <div className={`prompt-entry${draft ? ' has-value' : ''}`}>
              <textarea
                ref={textareaRef}
                rows={2}
                aria-label="Message"
                placeholder="Ask anything…"
                value={draft}
                onChange={event => setDraft(event.target.value)}
                onKeyDown={event => {
                  if (event.key === 'Enter' && !event.shiftKey && !event.nativeEvent.isComposing) {
                    event.preventDefault();
                    send();
                  }
                }}
              />
            </div>
            <div className="composer-bottom">
              <div className="attach-wrap">
                <button className="round-btn" type="button" aria-label="Add to message" tabIndex={-1}>
                  <PlusIcon />
                </button>
              </div>
              <div className="mode-wrap">
                <div className="mode-picker">
                  <div className="mode-trigger" aria-label="Work mode: Auto">
                    <span className="trigger-state trigger-auto">
                      <ShuffleIcon />
                      <span>Auto</span>
                    </span>
                    <ChevronDownIcon />
                  </div>
                </div>
              </div>
              <button className="round-btn" type="button" aria-label="Microphone" tabIndex={-1}>
                <MicIcon />
              </button>
              <button
                className="send"
                type="button"
                aria-label="Send"
                disabled={busy || !draft.trim()}
                onClick={event => {
                  event.stopPropagation();
                  send();
                }}
              >
                <ArrowUpIcon />
              </button>
            </div>
            <div className="suggestions" aria-label="Prompt suggestions">
              {SUGGESTIONS.map(({ label, Icon }) => (
                <button
                  type="button"
                  key={label}
                  onClick={event => {
                    event.stopPropagation();
                    setDraft(`${label}: `);
                    textareaRef.current?.focus();
                  }}
                >
                  <Icon />
                  <span>{label}</span>
                </button>
              ))}
            </div>
          </div>
          <div className="hint">NeoChat is AI and can make mistakes. So double check important information.</div>
        </div>
      </section>
    </div>
  );
}
