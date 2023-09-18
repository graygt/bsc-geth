// Copyright 2020 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package trie

import (
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/trie/trienode"
	"github.com/ethereum/go-ethereum/trie/triestate"
)

// leafChanSize is the size of the leafCh. It's a pretty arbitrary number, to allow
// some parallelism but not incur too much memory overhead.
const leafChanSize = 200

// leaf represents a trie leaf node
type leaf struct {
	blob   []byte      // raw blob of leaf
	parent common.Hash // the hash of parent node
}

type leafInfo struct {
	node   node        // the node to commit
	parent common.Hash // the hash of parent node
}

// committer is the tool used for the trie Commit operation. The committer will
// capture all dirty nodes during the commit process and keep them cached in
// insertion order.
type committer struct {
	nodes  *trienode.NodeSet
	tracer *tracer
	onleaf triestate.LeafCallback
	leafCh chan *leafInfo
}

// committers live in a global sync.Pool
var committerPool = sync.Pool{
	New: func() interface{} {
		return &committer{}
	},
}

// newCommitter creates a new committer or picks one from the pool.
func newCommitter(nodeset *trienode.NodeSet, tracer *tracer) *committer {
	committer := committerPool.Get().(*committer)
	committer.nodes = nodeset
	committer.tracer = tracer

	return committer
}

func returnCommitterToPool(h *committer) {
	h.nodes = nil
	h.tracer = nil
	h.onleaf = nil
	h.leafCh = nil
	committerPool.Put(h)
}

// Commit collapses a node down into a hash node.
func (c *committer) Commit(n node) hashNode {
	return c.commit(nil, n).(hashNode)
}

// commit collapses a node down into a hash node and returns it.
func (c *committer) commit(path []byte, n node) node {
	// if this path is clean, use available cached data
	hash, dirty := n.cache()
	if hash != nil && !dirty {
		return hash
	}
	// Commit children, then parent, and remove the dirty flag.
	switch cn := n.(type) {
	case *shortNode:
		// Commit child
		collapsed := cn.copy()

		// If the child is fullNode, recursively commit,
		// otherwise it can only be hashNode or valueNode.
		if _, ok := cn.Val.(*fullNode); ok {
			collapsed.Val = c.commit(append(path, cn.Key...), cn.Val)
		}
		// The key needs to be copied, since we're adding it to the
		// modified nodeset.
		collapsed.Key = hexToCompact(cn.Key)
		hashedNode := c.store(path, collapsed)
		if hn, ok := hashedNode.(hashNode); ok {
			return hn
		}
		return collapsed
	case *fullNode:
		hashedKids := c.commitChildren(path, cn)
		collapsed := cn.copy()
		collapsed.Children = hashedKids

		hashedNode := c.store(path, collapsed)
		if hn, ok := hashedNode.(hashNode); ok {
			return hn
		}
		return collapsed
	case hashNode:
		return cn
	default:
		// nil, valuenode shouldn't be committed
		panic(fmt.Sprintf("%T: invalid node: %v", n, n))
	}
}

// commitChildren commits the children of the given fullnode
func (c *committer) commitChildren(path []byte, n *fullNode) [17]node {
	var children [17]node
	for i := 0; i < 16; i++ {
		child := n.Children[i]
		if child == nil {
			continue
		}
		// If it's the hashed child, save the hash value directly.
		// Note: it's impossible that the child in range [0, 15]
		// is a valueNode.
		if hn, ok := child.(hashNode); ok {
			children[i] = hn
			continue
		}
		// Commit the child recursively and store the "hashed" value.
		// Note the returned node can be some embedded nodes, so it's
		// possible the type is not hashNode.
		children[i] = c.commit(append(path, byte(i)), child)
	}
	// For the 17th child, it's possible the type is valuenode.
	if n.Children[16] != nil {
		children[16] = n.Children[16]
	}
	return children
}

// store hashes the node n and adds it to the modified nodeset. If leaf collection
// is enabled, leaf nodes will be tracked in the modified nodeset as well.
func (c *committer) store(path []byte, n node) node {
	// Larger nodes are replaced by their hash and stored in the database.
	var hash, _ = n.cache()

	// This was not generated - must be a small node stored in the parent.
	// In theory, we should check if the node is leaf here (embedded node
	// usually is leaf node). But small value (less than 32bytes) is not
	// our target (leaves in account trie only).
	if hash == nil {
		// The node is embedded in its parent, in other words, this node
		// will not be stored in the database independently, mark it as
		// deleted only if the node was existent in database before.
		_, ok := c.tracer.accessList[string(path)]
		if ok {
			c.nodes.AddNode(path, trienode.NewDeleted())
		}
		return n
	}
	// Collect the dirty node to nodeset for return.
	nhash := common.BytesToHash(hash)
	c.nodes.AddNode(path, trienode.New(nhash, nodeToBytes(n)))

	// Collect the corresponding leaf node if it's required. We don't check
	// full node since it's impossible to store value in fullNode. The key
	// length of leaves should be exactly same.
	if c.leafCh != nil {
		c.leafCh <- &leafInfo{
			node:   n,
			parent: nhash,
		}
	} else {
		if sn, ok := n.(*shortNode); ok {
			if val, ok := sn.Val.(valueNode); ok {
				c.nodes.AddLeaf(nhash, val)
			}
		}
	}
	return hash
}

// commitLoop does the actual insert + leaf callback for nodes.
func (c *committer) commitLoop() {
	for item := range c.leafCh {
		var (
			n      = item.node
			parent = item.parent
		)

		if c.onleaf != nil {
			switch n := n.(type) {
			case *shortNode:
				if child, ok := n.Val.(valueNode); ok {
					c.nodes.AddLeaf(parent, child)
					c.onleaf(nil, nil, child, parent, nil)
				}
			case *fullNode:
				// For children in range [0, 15], it's impossible
				// to contain valueNode. Only check the 17th child.
				if n.Children[16] != nil {
					c.nodes.AddLeaf(parent, n.Children[16].(valueNode))
					c.onleaf(nil, nil, n.Children[16].(valueNode), parent, nil)
				}
			}
		}
	}
}

// mptResolver the children resolver in merkle-patricia-tree.
type mptResolver struct{}

// ForEach implements childResolver, decodes the provided node and
// traverses the children inside.
func (resolver mptResolver) ForEach(node []byte, onChild func(common.Hash)) {
	forGatherChildren(mustDecodeNodeUnsafe(nil, node), onChild)
}

// forGatherChildren traverses the node hierarchy and invokes the callback
// for all the hashnode children.
func forGatherChildren(n node, onChild func(hash common.Hash)) {
	switch n := n.(type) {
	case *shortNode:
		forGatherChildren(n.Val, onChild)
	case *fullNode:
		for i := 0; i < 16; i++ {
			forGatherChildren(n.Children[i], onChild)
		}
	case hashNode:
		onChild(common.BytesToHash(n))
	case valueNode, nil:
	default:
		panic(fmt.Sprintf("unknown node type: %T", n))
	}
}

// estimateSize estimates the size of an rlp-encoded node, without actually
// rlp-encoding it (zero allocs). This method has been experimentally tried, and with a trie
// with 1000 leafs, the only errors above 1% are on small shortnodes, where this
// method overestimates by 2 or 3 bytes (e.g. 37 instead of 35)
func estimateSize(n node) int {
	switch n := n.(type) {
	case *shortNode:
		// A short node contains a compacted key, and a value.
		return 3 + len(n.Key) + estimateSize(n.Val)
	case *fullNode:
		// A full node contains up to 16 hashes (some nils), and a key
		s := 3
		for i := 0; i < 16; i++ {
			if child := n.Children[i]; child != nil {
				s += estimateSize(child)
			} else {
				s++
			}
		}
		return s
	case valueNode:
		return 1 + len(n)
	case hashNode:
		return 1 + len(n)
	default:
		panic(fmt.Sprintf("node type %T", n))
	}
}
