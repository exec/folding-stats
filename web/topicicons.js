/**
 * One icon per topic, drawn to the house style: a 24×24 box, currentColor, and a
 * 1.75 stroke, so they inherit the card's ink and both themes without a second set.
 *
 * Line art throughout with two deliberate exceptions. A space invader is pixels or it
 * is not a space invader, and a pokéball needs its centre filled to read as one at
 * 26px. Everything else stays a stroke so the row of them looks like one family
 * rather than twenty-five separate decisions.
 *
 * Kept out of views.js because it is 25 paths and no logic, and only the topics page
 * ever asks for it.
 */

// path()      stroke-only shapes, the default
// solid()     filled shapes, for the two that need to be
const P = (d, extra = '') => `<path d="${d}"${extra ? ' ' + extra : ''}/>`;

const ICONS = {
  // A mortarboard: the board seen in perspective, with a tassel.
  universities:
    P('M2 8.5 12 4l10 4.5-10 4.5z') +
    P('M6 10.6v4.9c0 1.5 2.7 2.8 6 2.8s6-1.3 6-2.8v-4.9') +
    P('M21 9v5'),

  // A space invader. Pixels, because anything smoother is a different reference.
  gaming:
    `<g fill="currentColor" stroke="none">` +
    `<rect x="7" y="4" width="2" height="2"/><rect x="15" y="4" width="2" height="2"/>` +
    `<rect x="9" y="6" width="6" height="2"/>` +
    `<rect x="5" y="8" width="14" height="2"/>` +
    `<rect x="3" y="10" width="18" height="4"/>` +
    `<rect x="3" y="14" width="2" height="2"/><rect x="19" y="14" width="2" height="2"/>` +
    `<rect x="7" y="14" width="10" height="2"/>` +
    `<rect x="5" y="16" width="2" height="2"/><rect x="17" y="16" width="2" height="2"/>` +
    `<rect x="9" y="16" width="2" height="2"/><rect x="13" y="16" width="2" height="2"/>` +
    `</g>`,

  // Three faiths rather than one, small enough to sit together: a cross, a crescent
  // and a six-pointed star. Three is the most that stays legible at this size.
  faith:
    // A cross, a crescent and a hexagram, side by side on one baseline. Three is
    // already the limit: a fourth would put every glyph below four pixels wide.
    P('M4.6 6.4v11.2M1.6 10.2h6') +
    P('M13.9 7.1a5 5 0 1 0 0 9.8 5.9 5.9 0 0 1 0-9.8z') +
    P('M19.6 8.7l-2.9 5h5.8zM19.6 15.3l-2.9-5h5.8z'),

  // A briefcase.
  employers:
    P('M3 8.5h18v10.5H3z') +
    P('M9 8.5V6.2c0-.7.5-1.2 1.2-1.2h3.6c.7 0 1.2.5 1.2 1.2v2.3') +
    P('M3 13h18'),

  // A pokéball.
  fandom:
    P('M12 3.2a8.8 8.8 0 1 0 0 17.6 8.8 8.8 0 0 0 0-17.6z') +
    P('M3.2 12h5.6M15.2 12h5.6') +
    P('M12 9.2a2.8 2.8 0 1 0 0 5.6 2.8 2.8 0 0 0 0-5.6z', 'fill="currentColor"'),

  // A graphics card: board, fan, and the bracket it screws to.
  hardware:
    P('M3 6.5h16.5v11H3z') +
    P('M11.2 9.4a2.6 2.6 0 1 0 0 5.2 2.6 2.6 0 0 0 0-5.2z') +
    P('M19.5 8.5H22M19.5 12H22M19.5 15.5H22') +
    P('M6 17.5v3'),

  // A medical cross.
  health:
    P('M9.4 3.5h5.2v5.9h5.9v5.2h-5.9v5.9H9.4v-5.9H3.5V9.4h5.9z'),

  // A microphone, for the channels and the shows.
  creators:
    P('M12 3.5a2.6 2.6 0 0 0-2.6 2.6v5.4a2.6 2.6 0 0 0 5.2 0V6.1A2.6 2.6 0 0 0 12 3.5z') +
    P('M6.5 11a5.5 5.5 0 0 0 11 0') +
    P('M12 16.5V20.5M8.8 20.5h6.4'),

  // Two speech bubbles.
  forums:
    P('M3.5 5.5h12v8h-7l-3.5 3v-3h-1.5z') +
    P('M8.5 16.5v1h6l3.5 3v-3h2v-8h-4'),

  // Nodes with edges: a distributed job, and near enough a chain.
  distributed:
    P('M12 3.4a2.4 2.4 0 1 0 0 4.8 2.4 2.4 0 0 0 0-4.8z') +
    P('M5 15.8a2.4 2.4 0 1 0 0 4.8 2.4 2.4 0 0 0 0-4.8z') +
    P('M19 15.8a2.4 2.4 0 1 0 0 4.8 2.4 2.4 0 0 0 0-4.8z') +
    P('M10.4 7.7 6.3 15.5M13.6 7.7l4.1 7.8M7.4 18.2h9.2'),

  // A conical flask.
  research:
    P('M9.5 3.5h5M10.5 3.5v6L4.7 18.2c-.6 1 .1 2.3 1.3 2.3h12c1.2 0 1.9-1.3 1.3-2.3L13.5 9.5v-6') +
    P('M7.4 14.5h9.2'),

  // A terminal prompt. Covers the distributions and everything else that ships source.
  opensource:
    P('M3 5h18v14H3z') +
    P('M7 10l2.6 2.4L7 14.8M12.6 15.2h4.4'),

  // A candle.
  memorial:
    P('M12 2.6c1.9 1.9 2.8 3.3 2.8 4.6a2.8 2.8 0 0 1-5.6 0c0-1.3.9-2.7 2.8-4.6z') +
    P('M7.8 10.6h8.4v10.8H7.8z') +
    P('M7.8 14.6h8.4'),

  // A twenty-sided die.
  tabletop:
    P('M12 2.8 21 8v8l-9 5.2L3 16V8z') +
    P('M12 2.8 7.5 10h9zM7.5 10 3 8M16.5 10 21 8') +
    P('M7.5 10 12 21.2 16.5 10zM7.5 10 3 16M16.5 10 21 16'),

  // An aerial, broadcasting.
  radio:
    P('M12 9.4a1.8 1.8 0 1 0 0 3.6 1.8 1.8 0 0 0 0-3.6z') +
    P('M12 13v8M8.4 21h7.2') +
    P('M8.2 15A5.6 5.6 0 0 1 8.2 7.2M15.8 7.2a5.6 5.6 0 0 1 0 7.8') +
    P('M5.4 17.6a9.6 9.6 0 0 1 0-13.2M18.6 4.4a9.6 9.6 0 0 1 0 13.2'),

  // A rainbow. Grounded at both ends so it reads as an arc and not as signal bars.
  lgbtq:
    P('M3 19a9 9 0 0 1 18 0') +
    P('M6.4 19a5.6 5.6 0 0 1 11.2 0') +
    P('M9.8 19a2.2 2.2 0 0 1 4.4 0'),

  // A shield.
  service:
    P('M12 3 20 6v6c0 4.4-3.2 7.6-8 9.4C7.2 19.6 4 16.4 4 12V6z') +
    P('M12 8v7'),

  // A car, side on.
  motoring:
    P('M3.5 15.5v-3l2-.5 2.2-4.2c.3-.5.8-.8 1.4-.8h5.8c.6 0 1.1.3 1.4.8l2.2 4.2 2 .5v3z') +
    P('M7.6 15.5a2 2 0 1 0 0 4 2 2 0 0 0 0-4zM16.4 15.5a2 2 0 1 0 0 4 2 2 0 0 0 0-4z') +
    P('M5.5 12h13'),

  // A trophy, for the clubs people follow.
  sports:
    P('M7.5 3.5h9v5.2a4.5 4.5 0 0 1-9 0z') +
    P('M7.5 5H4.8v1.6A3.4 3.4 0 0 0 7.9 10M16.5 5h2.7v1.6A3.4 3.4 0 0 1 16.1 10') +
    P('M12 13.2v3.6M8.4 20.5h7.2l-.8-3.7H9.2z'),

  // A quaver.
  music:
    P('M9.5 18.2V5.2l9-1.7v13') +
    P('M6.6 15.4a2.9 2.9 0 1 0 0 5.8 2.9 2.9 0 0 0 0-5.8zM15.6 13.7a2.9 2.9 0 1 0 0 5.8 2.9 2.9 0 0 0 0-5.8z'),

  // A megaphone: activism rather than the ballot box, which is only half of it.
  politics:
    P('M4 10v4l3 .6 2 4.4h2.6l-1.4-4 9.8 3V6.6L4 10z') +
    P('M20 9.4a2.6 2.6 0 0 1 0 5.2'),

  // A wrench, for the makers.
  hobbies:
    P('M20 5.4a5.4 5.4 0 0 1-7 7.2l-6.6 6.6a2 2 0 0 1-2.8-2.8L10.2 10a5.4 5.4 0 0 1 7.2-7l-3.3 3.3.8 2.8 2.8.8z'),

  // A hard hat.
  professions:
    // A hard hat. The brim has to be wide and the dome shallow, or it reads as a bell.
    P('M2.2 16.4h19.6v2.6H2.2z') +
    P('M5.6 16.4v-3.1A6.4 6.4 0 0 1 12 6.9a6.4 6.4 0 0 1 6.4 6.4v3.1') +
    P('M12 6.9v-1.9') +
    P('M9.3 16.4V8.2M14.7 16.4V8.2'),

  // A rosette: a scout badge and a service-club pin are the same shape.
  clubs:
    P('M12 3.4a5.2 5.2 0 1 0 0 10.4 5.2 5.2 0 0 0 0-10.4z') +
    P('M9.2 13.4 7.4 20.6l4.6-2.4 4.6 2.4-1.8-7.2'),

  // The Happy Human, which is the humanist movement's own symbol.
  secular:
    P('M12 3.6a2.1 2.1 0 1 0 0 4.2 2.1 2.1 0 0 0 0-4.2z') +
    P('M4.5 20.4c0-5.4 3.4-9.2 7.5-9.2s7.5 3.8 7.5 9.2'),
};

/** topicIcon returns an <svg> for a topic slug, or null if it has none yet. */
export function topicIcon(slug) {
  const d = ICONS[slug];
  if (!d) return null;
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('class', 'topic-icon');
  svg.setAttribute('fill', 'none');
  svg.setAttribute('stroke', 'currentColor');
  svg.setAttribute('stroke-width', '1.75');
  svg.setAttribute('stroke-linecap', 'round');
  svg.setAttribute('stroke-linejoin', 'round');
  // Decorative: the topic's name is right beside it in text.
  svg.setAttribute('aria-hidden', 'true');
  svg.innerHTML = d;
  return svg;
}
