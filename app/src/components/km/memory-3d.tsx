/* eslint-disable react/no-unknown-property -- react-three-fiber three.js props */
import { Canvas, useFrame } from '@react-three/fiber/native';
import { useMemo, useRef } from 'react';
import { Gesture, GestureDetector, GestureHandlerRootView } from 'react-native-gesture-handler';
import { useSharedValue, type SharedValue } from 'react-native-reanimated';
import * as THREE from 'three';

import type { MemTreeNode } from '@/lib/api';
import { Text, View } from '@/tw';

import { KM } from './colors';

// MemNodeInfo is the data surfaced to the parent screen when a node is tapped,
// so the memory details can be rendered below the 3D view.
export type MemNodeInfo = { id: string; title: string; content?: string; depth: number };

type FlatNode = {
  id: string;
  title: string;
  content?: string;
  depth: number;
  pos: [number, number, number];
  color: string;
};
type FlatEdge = { from: [number, number, number]; to: [number, number, number]; color: string };

const CAT_HUES = ['#62b6c9', '#6fbf73', '#b48ef2', '#f2b43a', '#e5675c', '#7fb2ff', '#e59ecf', '#9fd68a'];

function fib(i: number, n: number, r: number): [number, number, number] {
  const phi = Math.acos(1 - (2 * (i + 0.5)) / Math.max(n, 1));
  const theta = Math.PI * (1 + Math.sqrt(5)) * (i + 0.5);
  return [r * Math.sin(phi) * Math.cos(theta), r * Math.sin(phi) * Math.sin(theta), r * Math.cos(phi)];
}

function layout(root: MemTreeNode | null): { nodes: FlatNode[]; edges: FlatEdge[] } {
  const nodes: FlatNode[] = [];
  const edges: FlatEdge[] = [];
  if (!root) return { nodes, edges };

  let catIdx = 0;
  const radiusFor = (depth: number) => (depth <= 1 ? 4.6 : 1.8);

  const walk = (node: MemTreeNode, pos: [number, number, number], depth: number, color: string) => {
    nodes.push({
      id: node.node_id ?? `n${nodes.length}`,
      title: node.title || node.node_id || 'node',
      content: node.content || node.summary,
      depth,
      pos,
      color,
    });
    const children = node.children ?? [];
    children.forEach((child, i) => {
      const childColor = depth === 0 ? CAT_HUES[catIdx++ % CAT_HUES.length] : color;
      const f = fib(i, children.length, radiusFor(depth + 1));
      const childPos: [number, number, number] =
        depth === 0 ? f : [pos[0] + f[0], pos[1] + f[1], pos[2] + f[2]];
      edges.push({ from: pos, to: childPos, color: childColor });
      walk(child, childPos, depth + 1, childColor);
    });
  };
  walk(root, [0, 0, 0], 0, KM.amber);
  return { nodes, edges };
}

const sizeFor = (d: number) => (d === 0 ? 0.6 : d === 1 ? 0.34 : 0.2);

function Graph({
  nodes,
  edges,
  rotX,
  rotY,
  zoom,
  onSelect,
}: {
  nodes: FlatNode[];
  edges: FlatEdge[];
  rotX: SharedValue<number>;
  rotY: SharedValue<number>;
  zoom: SharedValue<number>;
  onSelect: (n: FlatNode) => void;
}) {
  const group = useRef<THREE.Group>(null);
  const spin = useRef(0);

  useFrame((state, delta) => {
    spin.current += delta * 0.05; // auto-spin, kept separate from gesture rotation
    if (group.current) {
      group.current.rotation.y = rotY.value + spin.current;
      group.current.rotation.x = rotX.value;
    }
    state.camera.position.z = zoom.value;
  });

  const edgeGeom = useMemo(() => {
    const pos = new Float32Array(edges.length * 6);
    const col = new Float32Array(edges.length * 6);
    edges.forEach((e, i) => {
      pos.set([...e.from, ...e.to], i * 6);
      const c = new THREE.Color(e.color);
      col.set([c.r, c.g, c.b, c.r, c.g, c.b], i * 6);
    });
    const g = new THREE.BufferGeometry();
    g.setAttribute('position', new THREE.BufferAttribute(pos, 3));
    g.setAttribute('color', new THREE.BufferAttribute(col, 3));
    return g;
  }, [edges]);

  return (
    <group ref={group}>
      <lineSegments geometry={edgeGeom}>
        <lineBasicMaterial vertexColors transparent opacity={0.35} />
      </lineSegments>
      {nodes.map((n) => (
        <mesh key={n.id} position={n.pos} onClick={() => onSelect(n)}>
          <sphereGeometry args={[sizeFor(n.depth), 20, 20]} />
          <meshStandardMaterial color={n.color} emissive={n.color} emissiveIntensity={0.45} roughness={0.4} />
        </mesh>
      ))}
    </group>
  );
}

export function Memory3D({
  root,
  onSelect,
  height = 380,
}: {
  root: MemTreeNode | null;
  onSelect?: (n: MemNodeInfo | null) => void;
  height?: number;
}) {
  const { nodes, edges } = useMemo(() => layout(root), [root]);

  const rotX = useSharedValue(0.3);
  const rotY = useSharedValue(0);
  const zoom = useSharedValue(13);
  const startZoom = useSharedValue(13);

  const pan = Gesture.Pan().onChange((e) => {
    rotY.value += e.changeX * 0.006;
    rotX.value += e.changeY * 0.006;
  });
  const pinch = Gesture.Pinch()
    .onBegin(() => {
      startZoom.value = zoom.value;
    })
    .onUpdate((e) => {
      zoom.value = Math.min(30, Math.max(5, startZoom.value / e.scale));
    });
  const gesture = Gesture.Simultaneous(pan, pinch);

  const handleSelect = (n: FlatNode) =>
    onSelect?.({ id: n.id, title: n.title, content: n.content, depth: n.depth });

  return (
    <GestureHandlerRootView style={{ height }}>
      <GestureDetector gesture={gesture}>
        <View
          className="flex-1 overflow-hidden rounded-md border border-km-line"
          style={{ borderCurve: 'continuous' }}>
          <Canvas camera={{ position: [0, 0, 13], fov: 55 }} style={{ flex: 1, backgroundColor: KM.ink }}>
            <ambientLight intensity={0.75} />
            <pointLight position={[10, 10, 10]} intensity={1.4} />
            <pointLight position={[-10, -6, -8]} intensity={0.5} color="#7fb2ff" />
            <fog attach="fog" args={[KM.ink, 14, 30]} />
            <Graph nodes={nodes} edges={edges} rotX={rotX} rotY={rotY} zoom={zoom} onSelect={handleSelect} />
          </Canvas>

          <View className="absolute left-2 top-2 rounded bg-km-panel px-2 py-1">
            <Text className="font-mono text-[10px] text-km-muted">
              {`${nodes.length} nodes · drag · pinch · tap a node`}
            </Text>
          </View>
        </View>
      </GestureDetector>
    </GestureHandlerRootView>
  );
}
