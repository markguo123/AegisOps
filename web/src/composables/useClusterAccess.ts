import { computed, ref } from 'vue'
import { apiGet } from '@/api/request'

interface Rule {scopeType:'namespace'|'cluster'; namespaces:string[]|null; actions:string[]}
interface Accessible {id:string;name:string;status:string;access:{rules:Rule[]}}
export function useClusterAccess(){
 const clusters=ref<Accessible[]>([])
 const target=ref('')
 const selected=computed(()=>clusters.value.find(c=>c.id===target.value))
 const rules=computed(()=>selected.value?.access.rules??[])
 async function refresh(){
  try{const result=await apiGet<Accessible[]>('/auth/access');clusters.value=result.data??[]}
  catch{clusters.value=[]}
  if(!selected.value)target.value=clusters.value.length===1?clusters.value[0]!.id:''
 }
 return {clusters,target,selected,rules,refresh}
}
