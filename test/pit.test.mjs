import { test } from 'node:test';
import assert from 'node:assert/strict';
import { memoryStore } from '../web/pit.js';

const lanes = async s => Object.fromEntries((await s.list()).cards.map(c => [c.title, c.lane]));

test('cards land in the queue, marked by who added them', async () => {
  const s = memoryStore();
  await s.add([{ title: 'mine' }, { title: 'theirs', source: 'agent', author: '@shop' }]);
  const { cards } = await s.list();
  assert.deepEqual(cards.map(c => [c.title, c.lane, c.source]), [['mine', 'unsorted', 'me'], ['theirs', 'unsorted', 'agent']]);
  assert.equal(cards[1].author, '@shop');
});

test('a vote moves the card and undo walks the log back, newest first', async () => {
  const s = memoryStore();
  const [a, b] = await s.add([{ title: 'a' }, { title: 'b' }]);
  await s.vote(a.id, 'in');
  await s.vote(b.id, 'out');
  assert.deepEqual(await lanes(s), { a: 'in', b: 'out' });
  assert.equal((await s.undo()).title, 'b');
  assert.deepEqual(await lanes(s), { a: 'in', b: 'unsorted' });
  await s.undo();
  assert.deepEqual(await lanes(s), { a: 'unsorted', b: 'unsorted' });
  assert.equal(await s.undo(), null);
  assert.equal(s.state().log.length, 2);
  assert.ok(s.state().log.every(x => x.voided));
});

test('a reason lives only in out, and undo restores the one it replaced', async () => {
  const s = memoryStore();
  const [a] = await s.add([{ title: 'a' }]);
  await s.place(a.id, { lane: 'out', reason: 'done' });
  assert.equal((await s.get(a.id)).reason, 'done');
  await s.place(a.id, { lane: 'in', reason: 'done' });
  assert.equal((await s.get(a.id)).reason, '');
  await s.undo();
  assert.equal((await s.get(a.id)).reason, 'done');
});

test('every write is saved, and replace redraws subscribers', async () => {
  const saved = [];
  const s = memoryStore({}, { save: st => saved.push(st) });
  const [a] = await s.add([{ title: 'a' }]);
  await s.vote(a.id, 'maybe');
  await s.comment(a.id, 'why not', 'agent');
  assert.equal(saved.length, 3);
  assert.equal(saved.at(-1).cards[0].comments[0].who, 'agent');
  let told = 0;
  s.subscribe(() => told++);
  s.replace({ cards: [{ id: 'x', title: 'x', lane: 'in', pos: 1 }] });
  assert.equal(told, 1);
  assert.deepEqual(await lanes(s), { x: 'in' });
});

test('starting cards get an id, the queue and an order', async () => {
  const s = memoryStore({ cards: [{ title: 'a' }, { title: 'b', lane: 'in' }] });
  const { cards } = await s.list();
  assert.ok(cards.every(c => c.id && typeof c.pos === 'number'));
  assert.deepEqual(cards.map(c => c.lane), ['unsorted', 'in']);
});
