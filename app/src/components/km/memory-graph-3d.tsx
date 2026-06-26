/* eslint-disable react/no-unknown-property -- react-three-fiber three.js props */
import { Canvas, useFrame } from '@react-three/fiber/native';
import { useMemo, useRef } from 'react';
import { Gesture, GestureDetector, GestureHandlerRootView } from 'react-native-gesture-handler';
import { useSharedValue, type SharedValue } from 'react-native-reanimated';
import * as THREE from 'three';

import type { GraphLink, GraphNode } from '@/lib/api';
import { Text, View } from '@/tw';

import { KM } from './colors';

export type GraphNodeInfo = { id: string; title: string; content?: string };

const CAT_HUES = ['#62b6c9', '#6fbf73', '#b48ef2', '#f2b43a', '#e5675c', '#7fb2ff', '#e59ecf', '#9fd68a'];

function hueFor(key: string): string {
  let h = 0;
  for (let i = 0; i < key.length; i++) h = (h * 31 + key.charCodeAt(i)) >>> 0;
  return CAT_HUES[h % CAT_HUES.length];
}

function fib(i: number, n: number, r: number): [number, number, number] {
  const phi = Math.acos(1 - (2 * (i + 0.5)) / Math.max(n, 1));
  const theta = Math.PI * (1 + Math.sqrt(5)) * (i + 0.5);
  return [r * Math.sin(phi) * Math.cos(theta), r * Math.sin(phi) * Math.sin(theta), r * Math.cos(phi)];
}

type Vec3 = [number, number, number];
type Placed = { node: GraphNode; pos: Vec3; color: string };

function Graph({
  placed,
  links,
  posById,
  rotX,
  rotY,
  zoom,
  onSelect,
}: {
  placed: Placed[];
  links: GraphLink[];
  posById: Record<string, Vec3>;
  rotX: SharedValue<number>;
  rotY: SharedValue<number>;
  zoom: SharedValue<number>;
  onSelect: (n: GraphNode) => void;
}) {
  const group = useRef<THREE.Group>(null);
  const spin = useRef(0);

  useFrame((state, delta) => {
    spin.current += delta * 0.04;
    if (group.current) {
      group.current.rotation.y = rotY.value + spin.current;
      group.current.rotation.x = rotX.value;
    }
    state.camera.position.z = zoom.value;
  });

  const edgeGeom = useMemo(() => {
    const valid = links.filter((l) => posById[l.from] && posById[l.to]);
    const pos = new Float32Array(valid.length * 6);
    const col = new Float32Array(valid.length * 6);
    valid.forEach((l, i) => {
      const a = posById[l.from];
      const b = posById[l.to];
      pos.set([...a, ...b], i * 6);
      const c = new THREE.Color(hueFor(l.relation || 'rel'));
      col.set([c.r, c.g, c.b, c.r, c.g, c.b], i * 6);
    });
    const g = new THREE.BufferGeometry();
    g.setAttribute('position', new THREE.BufferAttribute(pos, 3));
    g.setAttribute('color', new THREE.BufferAttribute(col, 3));
    return g;
  }, [links, posById]);

  return (
    <group ref={group}>
      <lineSegments geometry={edgeGeom}>
        <lineBasicMaterial vertexColors transparent opacity={0.5} />
      </lineSegments>
      {placed.map((p) => (
        <mesh key={p.node.id} position={p.pos} onClick={() => onSelect(p.node)}>
          <sphereGeometry args={[0.26, 18, 18]} />
          <meshStandardMaterial color={p.color} emissive={p.color} emissiveIntensity={0.45} roughness={0.4} />
        </mesh>
      ))}
    </group>
  );
}

export function MemoryGraph3D({
  nodes,
  links,
  onSelect,
  height = 380,
}: {
  nodes: GraphNode[];
  links: GraphLink[];
  onSelect?: (n: GraphNodeInfo) => void;
  height?: number;
}) {
  const { placed, posById } = useMemo(() => {
    const catColor: Record<string, string> = {};
    let ci = 0;
    const placedNodes: Placed[] = nodes.map((node, i) => {
      const cat = node.category || 'context';
      if (!(cat in catColor)) catColor[cat] = CAT_HUES[ci++ % CAT_HUES.length];
      return { node, pos: fib(i, nodes.length, 6), color: catColor[cat] };
    });
    const byId: Record<string, Vec3> = {};
    placedNodes.forEach((p) => {
      byId[p.node.id] = p.pos;
    });
    return { placed: placedNodes, posById: byId };
  }, [nodes]);

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

  return (
    <GestureHandlerRootView style={{ height }}>
      <GestureDetector gesture={gesture}>
        <View
          className="flex-1 overflow-hidden rounded-md border border-km-line"
          style={{ borderCurve: 'continuous' }}>
          <Canvas camera={{ position: [0, 0, 13], fov: 55 }} style={{ flex: 1, backgroundColor: KM.ink }}>
            <ambientLight intensity={0.75} />
            <pointLight position={[10, 10, 10]} intensity={1.4} />
            <fog attach="fog" args={[KM.ink, 14, 30]} />
            <Graph
              placed={placed}
              links={links}
              posById={posById}
              rotX={rotX}
              rotY={rotY}
              zoom={zoom}
              onSelect={(n) => onSelect?.({ id: n.id, title: n.title, content: n.content })}
            />
          </Canvas>
          <View className="absolute left-2 top-2 rounded bg-km-panel px-2 py-1">
            <Text className="font-mono text-[10px] text-km-muted">
              {`${nodes.length} memories · ${links.length} links · drag · pinch`}
            </Text>
          </View>
        </View>
      </GestureDetector>
    </GestureHandlerRootView>
  );
}
