function p(r){return r.map(e=>{const a=e.decisions.length,o=new Set(e.decisions.map(n=>n.approver));return{...e,decided:a,pending:e.approvers.filter(n=>!o.has(n))}})}export{p as a};
